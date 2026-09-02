package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

const (
	uniqueClaimAttempts = 32
	uniqueClaimLease    = 30 * time.Second
)

type uniqueReservation struct {
	fingerprint    string
	revision       uint64
	previous       *kv.UniqueClaim
	cancelPrevious bool
}

func (b *NATSBackend) reserveUnique(
	ctx context.Context,
	job *core.Job,
	now time.Time,
) (*uniqueReservation, *core.Job, error) {
	if job.Unique == nil {
		return nil, nil, nil
	}
	if err := validateUniquePolicy(job.Unique); err != nil {
		return nil, nil, err
	}

	fingerprint := kv.ComputeFingerprint(job)
	conflictPolicy := job.Unique.OnConflict
	if conflictPolicy == "" {
		conflictPolicy = "reject"
	}

	for attempt := 0; attempt < uniqueClaimAttempts; attempt++ {
		revision, err := b.unique.CreateClaim(ctx, fingerprint, job.ID, now)
		if err == nil {
			return &uniqueReservation{fingerprint: fingerprint, revision: revision}, nil, nil
		}
		if !errors.Is(err, jetstream.ErrKeyExists) {
			return nil, nil, fmt.Errorf("create unique claim: %w", err)
		}

		existingClaim, existingRevision, err := b.unique.GetClaim(ctx, fingerprint)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				continue
			}
			return nil, nil, fmt.Errorf("read unique claim: %w", err)
		}

		existingJob, infoErr := b.Info(ctx, existingClaim.JobID)
		pending := infoErr != nil && uniqueClaimIsPending(existingClaim, now)
		relevant := infoErr == nil && uniqueJobIsRelevant(existingJob, job.Unique, now)
		if pending || relevant {
			switch conflictPolicy {
			case "reject":
				return nil, nil, duplicateUniqueError(existingClaim.JobID, fingerprint)
			case "ignore":
				if infoErr == nil {
					existingJob.IsExisting = true
					return nil, existingJob, nil
				}
				if err := waitForUniqueOwner(ctx); err != nil {
					return nil, nil, err
				}
				continue
			case "replace":
				if pending {
					// A replacement that has not finished persistence owns the
					// claim lease. Do not let a racing request steal it.
					return nil, nil, duplicateUniqueError(existingClaim.JobID, fingerprint)
				}
				newClaim := kv.UniqueClaim{JobID: job.ID, ClaimedAt: core.FormatTime(now)}
				revision, replaceErr := b.unique.ReplaceClaim(ctx, fingerprint, newClaim, existingRevision)
				if replaceErr != nil {
					if errors.Is(replaceErr, jetstream.ErrKeyExists) {
						continue
					}
					return nil, nil, fmt.Errorf("replace unique claim: %w", replaceErr)
				}
				previous := existingClaim
				return &uniqueReservation{
					fingerprint:    fingerprint,
					revision:       revision,
					previous:       &previous,
					cancelPrevious: true,
				}, nil, nil
			}
		}

		newClaim := kv.UniqueClaim{JobID: job.ID, ClaimedAt: core.FormatTime(now)}
		revision, replaceErr := b.unique.ReplaceClaim(ctx, fingerprint, newClaim, existingRevision)
		if replaceErr != nil {
			if errors.Is(replaceErr, jetstream.ErrKeyExists) {
				continue
			}
			return nil, nil, fmt.Errorf("reacquire unique claim: %w", replaceErr)
		}
		previous := existingClaim
		return &uniqueReservation{
			fingerprint: fingerprint,
			revision:    revision,
			previous:    &previous,
		}, nil, nil
	}

	return nil, nil, core.NewConflictError(
		"Unique job ownership changed concurrently; retry the enqueue operation.",
		map[string]any{"job_id": job.ID, "unique_key": fingerprint},
	)
}

func (b *NATSBackend) rollbackUnique(ctx context.Context, reservation *uniqueReservation) {
	if reservation == nil {
		return
	}
	if reservation.previous == nil {
		_ = b.unique.ReleaseClaim(ctx, reservation.fingerprint, reservation.revision)
		return
	}
	_, _ = b.unique.ReplaceClaim(ctx, reservation.fingerprint, *reservation.previous, reservation.revision)
}

func (b *NATSBackend) cancelReplacedUnique(ctx context.Context, reservation *uniqueReservation) {
	if reservation == nil || !reservation.cancelPrevious || reservation.previous == nil {
		return
	}
	if _, err := b.Cancel(ctx, reservation.previous.JobID); err != nil {
		var ojsErr *core.OJSError
		if errors.As(err, &ojsErr) &&
			(ojsErr.Code == core.ErrCodeConflict || ojsErr.Code == core.ErrCodeNotFound) {
			return
		}
		slog.Warn("unique replacement could not cancel previous job",
			"job_id", reservation.previous.JobID,
			"error", err,
		)
	}
}

func validateUniquePolicy(policy *core.UniquePolicy) error {
	switch policy.OnConflict {
	case "", "reject", "ignore", "replace":
	default:
		return core.NewInvalidRequestError(
			fmt.Sprintf("Unsupported unique on_conflict policy %q.", policy.OnConflict),
			map[string]any{"field": "unique.on_conflict", "received": policy.OnConflict},
		)
	}
	for _, key := range policy.Keys {
		switch key {
		case "type", "queue", "args":
		default:
			return core.NewInvalidRequestError(
				fmt.Sprintf("Unsupported unique key %q.", key),
				map[string]any{"field": "unique.keys", "received": key},
			)
		}
	}
	if policy.Period != "" {
		period, err := core.ParseISO8601Duration(policy.Period)
		if err != nil {
			if parsed, parseErr := time.ParseDuration(policy.Period); parseErr == nil {
				period = parsed
			} else {
				return core.NewInvalidRequestError(
					fmt.Sprintf("Invalid unique period %q.", policy.Period),
					map[string]any{"field": "unique.period", "received": policy.Period},
				)
			}
		}
		if period <= 0 {
			return core.NewInvalidRequestError(
				"Unique period must be greater than zero.",
				map[string]any{"field": "unique.period"},
			)
		}
	}
	for _, state := range policy.States {
		switch state {
		case core.StateScheduled, core.StateAvailable, core.StatePending, core.StateActive,
			core.StateCompleted, core.StateRetryable, core.StateCancelled, core.StateDiscarded:
		default:
			return core.NewInvalidRequestError(
				fmt.Sprintf("Invalid unique state %q.", state),
				map[string]any{"field": "unique.states", "received": state},
			)
		}
	}
	return nil
}

func uniqueClaimIsPending(claim kv.UniqueClaim, now time.Time) bool {
	if claim.ClaimedAt == "" {
		return false
	}
	claimedAt, err := time.Parse(time.RFC3339, claim.ClaimedAt)
	return err == nil && now.Sub(claimedAt) < uniqueClaimLease
}

func uniqueJobIsRelevant(existing *core.Job, policy *core.UniquePolicy, now time.Time) bool {
	if policy.Period != "" {
		period, err := core.ParseISO8601Duration(policy.Period)
		if err != nil {
			period, _ = time.ParseDuration(policy.Period)
		}
		createdAt, err := time.Parse(time.RFC3339, existing.CreatedAt)
		if err == nil && !now.Before(createdAt.Add(period)) {
			return false
		}
	}
	if len(policy.States) == 0 {
		return !core.IsTerminalState(existing.State)
	}
	for _, state := range policy.States {
		if state == existing.State {
			return true
		}
	}
	return false
}

func duplicateUniqueError(existingID, fingerprint string) *core.OJSError {
	return &core.OJSError{
		Code:    core.ErrCodeDuplicate,
		Message: "A job with the same unique key already exists.",
		Details: map[string]any{
			"existing_job_id": existingID,
			"unique_key":      fingerprint,
		},
	}
}

func waitForUniqueOwner(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
