package nats

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/robfig/cron/v3"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
	"github.com/openjobspec/ojs-backend-nats/internal/kv"
)

const (
	cronClaimPrefix            = "cron-occurrence."
	cronClaimFilter            = cronClaimPrefix + ">"
	cronClaimLease             = time.Second
	cronDeleteRetryInterval    = 10 * time.Millisecond
	cronLegacyMaintenanceLimit = 256
	// NATS MaxPayload includes serialized headers; KV CAS headers remain below this bound.
	cronKVHeaderSafetyMargin = 96
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

type cronOccurrenceClaim struct {
	CronName     string `json:"cron_name"`
	OccurrenceAt string `json:"occurrence_at"`
	JobID        string `json:"job_id,omitempty"`
	Status       string `json:"status"`
	Phase        string `json:"phase,omitempty"`
	Owner        string `json:"owner"`
	LeaseUntil   string `json:"lease_until"`
	ClaimedAt    string `json:"claimed_at"`
	CronRevision uint64 `json:"cron_revision,omitempty"`
}

// RegisterCron registers a cron job.
func (b *NATSBackend) RegisterCron(ctx context.Context, cronJob *core.CronJob) (*core.CronJob, error) {
	if cronJob == nil {
		return nil, core.NewInvalidRequestError("Cron registration is required.", nil)
	}
	expr := cronJob.Expression
	if expr == "" {
		expr = cronJob.Schedule
	}
	schedule, timezone, err := parseCronSchedule(expr, cronJob.Timezone)
	if err != nil {
		return nil, core.NewInvalidRequestError(
			fmt.Sprintf("Invalid cron schedule: %v", err),
			map[string]any{"expression": expr, "timezone": cronJob.Timezone, "error": err.Error()},
		)
	}

	normalizeCronTemplate(cronJob)
	if cronJob.JobTemplate == nil || cronJob.JobTemplate.Type == "" {
		return nil, core.NewInvalidRequestError(
			"Cron job_template.type is required.",
			map[string]any{"field": "job_template.type", "validation": "required"},
		)
	}
	if cronJob.JobTemplate.Args == nil {
		cronJob.JobTemplate.Args = json.RawMessage(`[]`)
	}
	if err := core.ValidateEnqueueRequest(&core.EnqueueRequest{
		Type:    cronJob.JobTemplate.Type,
		Args:    cronJob.JobTemplate.Args,
		Options: cronJob.JobTemplate.Options,
	}); err != nil {
		return nil, err
	}

	now := time.Now()
	cronJob.CreatedAt = core.FormatTime(now)
	cronJob.NextRunAt = core.FormatTime(schedule.Next(now))
	cronJob.Schedule = expr
	cronJob.Expression = expr
	cronJob.Timezone = timezone
	if cronJob.Queue == "" {
		cronJob.Queue = "default"
	}
	if cronJob.OverlapPolicy == "" {
		cronJob.OverlapPolicy = "allow"
	}
	switch cronJob.OverlapPolicy {
	case "allow", "skip":
	default:
		return nil, core.NewInvalidRequestError(
			fmt.Sprintf("Invalid overlap_policy %q.", cronJob.OverlapPolicy),
			map[string]any{"field": "overlap_policy", "received": cronJob.OverlapPolicy},
		)
	}
	cronJob.Enabled = true

	if err := b.validateCronPayloads(cronJob); err != nil {
		return nil, err
	}
	if err := b.cronStore.Register(ctx, cronJob); err != nil {
		return nil, fmt.Errorf("register cron: %w", err)
	}
	return cronJob, nil
}

// ListCron lists all registered cron jobs.
func (b *NATSBackend) ListCron(ctx context.Context) ([]*core.CronJob, error) {
	return b.cronStore.List(ctx)
}

// DeleteCron removes a cron job.
func (b *NATSBackend) DeleteCron(ctx context.Context, name string) (*core.CronJob, error) {
	entry, err := b.cronStore.GetEntry(ctx, name)
	if err != nil {
		return nil, core.NewNotFoundError("Cron job", name)
	}

	deletedCron := *entry.Cron
	entry.Cron.Enabled = false
	if _, err := b.cronStore.Update(ctx, entry.Cron, entry.Revision); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, core.NewConflictError(
				"Cron registration changed concurrently.",
				map[string]any{"cron_name": name},
			)
		}
		return nil, fmt.Errorf("disable cron before delete: %w", err)
	}

	deadline := time.Now().Add(cronClaimLease + 2*cronDeleteRetryInterval)
	for {
		remaining, err := b.reconcileCronClaimsFor(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("reconcile cron claims before delete: %w", err)
		}
		if remaining == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			return nil, core.NewConflictError(
				"Cron occurrence reconciliation is still in flight.",
				map[string]any{"cron_name": name, "remaining_claims": remaining},
			)
		}
		timer := time.NewTimer(cronDeleteRetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}

	current, err := b.cronStore.GetEntry(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return &deletedCron, nil
		}
		return nil, fmt.Errorf("read disabled cron before delete: %w", err)
	}
	if current.Cron.Enabled || current.Cron.CreatedAt != deletedCron.CreatedAt {
		return nil, core.NewConflictError(
			"Cron registration changed concurrently.",
			map[string]any{"cron_name": name},
		)
	}
	if err := b.cronStore.DeleteRevision(ctx, name, current.Revision); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return nil, core.NewConflictError(
				"Cron registration changed concurrently.",
				map[string]any{"cron_name": name},
			)
		}
		return nil, fmt.Errorf("delete cron: %w", err)
	}
	return &deletedCron, nil
}

// FireCronJobs claims and reconciles every due cron occurrence.
func (b *NATSBackend) FireCronJobs(ctx context.Context) error {
	if err := b.reconcileHandoffs(ctx); err != nil {
		return err
	}
	maintenanceErr := b.maintainLegacyCronClaims(ctx, cronLegacyMaintenanceLimit)
	if err := b.reconcileCronClaims(ctx); err != nil {
		return err
	}

	entries, err := b.cronStore.ListEntries(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	firstErr := maintenanceErr
	for _, listed := range entries {
		for catchup := 0; catchup < 100; catchup++ {
			entry, err := b.cronStore.GetEntry(ctx, listed.Cron.Name)
			if err != nil || !entry.Cron.Enabled || entry.Cron.NextRunAt == "" {
				break
			}
			occurrence, ok := parseIndexTimestamp(entry.Cron.NextRunAt)
			if !ok || now.Before(occurrence) {
				break
			}
			if err := b.claimCronOccurrence(ctx, entry.Cron, occurrence, entry.Revision); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				break
			}
			updated, err := b.cronStore.GetEntry(ctx, listed.Cron.Name)
			if err != nil || updated.Cron.NextRunAt == entry.Cron.NextRunAt {
				break
			}
		}
	}
	return firstErr
}

func (b *NATSBackend) claimCronOccurrence(
	ctx context.Context,
	registration *core.CronJob,
	occurrence time.Time,
	registrationRevision uint64,
) error {
	now := time.Now()
	status := "pending"
	jobID := core.NewUUIDv7()
	if registration.OverlapPolicy == "skip" && b.isCronInstanceRunning(ctx, registration.Name) {
		status = "skipped"
		jobID = ""
	}
	claim := cronOccurrenceClaim{
		CronName:     registration.Name,
		OccurrenceAt: core.FormatTime(occurrence),
		JobID:        jobID,
		Status:       status,
		Phase:        "preparing",
		Owner:        b.instanceID,
		LeaseUntil:   core.FormatTime(now.Add(cronClaimLease)),
		ClaimedAt:    core.FormatTime(now),
		CronRevision: registrationRevision,
	}
	data, err := json.Marshal(&claim)
	if err != nil {
		return err
	}
	key := cronOccurrenceKey(registration.Name, occurrence)
	revision, err := b.cronClaims.Create(ctx, key, data)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return b.reconcileCronClaim(ctx, key)
		}
		return fmt.Errorf("claim cron occurrence: %w", err)
	}

	current, err := b.cronRegistrationMatches(ctx, registration.Name, occurrence, registrationRevision)
	if err != nil {
		return err
	}
	if !current {
		if err := b.cronClaims.DeleteRevision(ctx, key, revision); err != nil &&
			!errors.Is(err, jetstream.ErrKeyExists) &&
			!errors.Is(err, jetstream.ErrKeyNotFound) &&
			!errors.Is(err, jetstream.ErrKeyDeleted) {
			return err
		}
		return nil
	}

	claim.Phase = ""
	if _, err := b.updateCronClaim(ctx, key, &claim, revision); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return b.reconcileCronClaim(ctx, key)
		}
		return err
	}
	return b.reconcileCronClaim(ctx, key)
}

func (b *NATSBackend) reconcileCronClaims(ctx context.Context) error {
	keys, err := b.cronClaimKeys(ctx)
	if err != nil {
		return err
	}
	var firstErr error
	for _, key := range keys {
		if err := b.reconcileCronClaim(ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *NATSBackend) reconcileCronClaimsFor(ctx context.Context, name string) (int, error) {
	keys, err := b.cronClaimKeys(ctx)
	if err != nil {
		return 0, err
	}
	var firstErr error
	remaining := 0
	for _, key := range keys {
		claim, _, err := b.getCronClaim(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				continue
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if claim.CronName != name {
			continue
		}
		if err := b.reconcileCronClaim(ctx, key); err != nil && firstErr == nil {
			firstErr = err
		}
		if b.cronClaims.Exists(ctx, key) {
			remaining++
		}
	}
	return remaining, firstErr
}

func (b *NATSBackend) cronClaimKeys(ctx context.Context) ([]string, error) {
	return b.cronClaims.KeysFiltered(ctx, cronClaimFilter)
}

func (b *NATSBackend) reconcileCronClaim(ctx context.Context, key string) error {
	for attempt := 0; attempt < 32; attempt++ {
		claim, revision, err := b.getCronClaim(ctx, key)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				return nil
			}
			return err
		}
		if claim.Status == "completed" {
			retry, err := b.reconcileCompletedCronClaim(ctx, key, &claim, revision)
			if retry {
				continue
			}
			return err
		}

		now := time.Now()
		leaseUntil, _ := time.Parse(time.RFC3339, claim.LeaseUntil)
		if claim.Phase == "preparing" {
			if now.Before(leaseUntil) {
				return nil
			}
			occurrence, ok := parseIndexTimestamp(claim.OccurrenceAt)
			if !ok {
				return fmt.Errorf("invalid claimed cron occurrence %q", claim.OccurrenceAt)
			}
			current, err := b.cronRegistrationMatches(
				ctx,
				claim.CronName,
				occurrence,
				claim.CronRevision,
			)
			if err != nil {
				return err
			}
			if !current {
				if err := b.cronClaims.DeleteRevision(ctx, key, revision); err != nil {
					if errors.Is(err, jetstream.ErrKeyExists) {
						continue
					}
					if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
						return nil
					}
					return err
				}
				return nil
			}
			claim.Phase = ""
			claim.Owner = b.instanceID
			claim.LeaseUntil = core.FormatTime(now.Add(cronClaimLease))
			_, err = b.updateCronClaim(ctx, key, &claim, revision)
			if err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return err
			}
			continue
		}
		if claim.Owner != b.instanceID && now.Before(leaseUntil) {
			return nil
		}
		if claim.Owner != b.instanceID || !now.Before(leaseUntil) {
			claim.Owner = b.instanceID
			claim.LeaseUntil = core.FormatTime(now.Add(cronClaimLease))
			revision, err = b.updateCronClaim(ctx, key, &claim, revision)
			if err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				return err
			}
		}

		registration, current, err := b.currentCronRegistrationForClaim(ctx, &claim)
		if err != nil {
			return err
		}
		if !current {
			if err := b.cronClaims.DeleteRevision(ctx, key, revision); err != nil {
				if errors.Is(err, jetstream.ErrKeyExists) {
					continue
				}
				if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
					return nil
				}
				return err
			}
			return nil
		}

		if claim.Status == "pending" {
			revision, err = b.ensureCronOccurrenceJob(ctx, key, &claim, registration, revision)
			if err != nil {
				return err
			}
		}
		occurrence, ok := parseIndexTimestamp(claim.OccurrenceAt)
		if !ok {
			return fmt.Errorf("invalid claimed cron occurrence %q", claim.OccurrenceAt)
		}
		advanced, err := b.advanceCronCursor(
			ctx,
			claim.CronName,
			occurrence,
			claim.CronRevision,
		)
		if err != nil {
			return err
		}
		if !advanced {
			return nil
		}

		claim.Status = "completed"
		claim.LeaseUntil = core.FormatTime(time.Now())
		completedRevision, err := b.updateCronClaim(ctx, key, &claim, revision)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			return err
		}
		if err := b.cronClaims.DeleteRevision(ctx, key, completedRevision); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				return nil
			}
			return err
		}
		return nil
	}
	return core.NewConflictError(
		"Cron occurrence changed concurrently.",
		map[string]any{"claim_key": key},
	)
}

func (b *NATSBackend) reconcileCompletedCronClaim(
	ctx context.Context,
	key string,
	claim *cronOccurrenceClaim,
	revision uint64,
) (bool, error) {
	occurrence, ok := parseIndexTimestamp(claim.OccurrenceAt)
	if !ok {
		return false, fmt.Errorf("invalid claimed cron occurrence %q", claim.OccurrenceAt)
	}
	advanced, err := b.cronCursorPastOccurrence(ctx, claim.CronName, occurrence)
	if err != nil || !advanced {
		return false, err
	}
	if err := b.cronClaims.DeleteRevision(ctx, key, revision); err != nil {
		if errors.Is(err, jetstream.ErrKeyExists) {
			return true, nil
		}
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return false, nil
		}
		return false, err
	}
	return false, nil
}

func (b *NATSBackend) ensureCronOccurrenceJob(
	ctx context.Context,
	key string,
	claim *cronOccurrenceClaim,
	registration *core.CronJob,
	revision uint64,
) (uint64, error) {
	if claim.JobID == "" {
		return revision, nil
	}
	if _, err := b.Info(ctx, claim.JobID); err == nil {
		if b.stats.Exists(ctx, handoffKey(claim.JobID)) {
			if err := b.reconcileHandoff(ctx, handoffKey(claim.JobID)); err != nil {
				return revision, err
			}
		}
		if registration.OverlapPolicy == "skip" {
			b.setCronInstance(ctx, claim.CronName, claim.JobID)
		}
		return revision, nil
	}

	job := cronRegistrationToJob(registration, claim.JobID)
	result, err := b.Push(ctx, job)
	if result != nil && result.ID != "" && result.ID != claim.JobID {
		claim.JobID = result.ID
		revision, err = b.updateCronClaim(ctx, key, claim, revision)
		if err != nil {
			return revision, err
		}
	}
	if err != nil {
		if _, infoErr := b.Info(ctx, claim.JobID); infoErr != nil {
			return revision, err
		}
		if b.stats.Exists(ctx, handoffKey(claim.JobID)) {
			if reconcileErr := b.reconcileHandoff(ctx, handoffKey(claim.JobID)); reconcileErr != nil {
				return revision, reconcileErr
			}
		}
	}
	if registration.OverlapPolicy == "skip" {
		b.setCronInstance(ctx, claim.CronName, claim.JobID)
	}
	return revision, nil
}

func (b *NATSBackend) advanceCronCursor(
	ctx context.Context,
	name string,
	occurrence time.Time,
	claimRevision uint64,
) (bool, error) {
	for attempt := 0; attempt < 32; attempt++ {
		entry, err := b.cronStore.GetEntry(ctx, name)
		if err != nil {
			if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
				return false, nil
			}
			return false, err
		}
		currentOccurrence, ok := parseIndexTimestamp(entry.Cron.NextRunAt)
		if !ok {
			return false, fmt.Errorf("invalid cron cursor %q for %s", entry.Cron.NextRunAt, name)
		}
		if currentOccurrence.After(occurrence) {
			return true, nil
		}
		if currentOccurrence.Before(occurrence) {
			return false, nil
		}
		if claimRevision != 0 && entry.Revision != claimRevision {
			return false, nil
		}
		schedule, _, err := parseCronSchedule(entry.Cron.Expression, entry.Cron.Timezone)
		if err != nil {
			return false, err
		}
		entry.Cron.LastRunAt = core.FormatTime(occurrence)
		entry.Cron.NextRunAt = core.FormatTime(schedule.Next(occurrence))
		if _, err := b.cronStore.Update(ctx, entry.Cron, entry.Revision); err != nil {
			if errors.Is(err, jetstream.ErrKeyExists) {
				continue
			}
			return false, err
		}
		return b.cronCursorPastOccurrence(ctx, name, occurrence)
	}
	return false, core.NewConflictError(
		"Cron cursor changed concurrently.",
		map[string]any{"cron_name": name, "occurrence": core.FormatTime(occurrence)},
	)
}

func (b *NATSBackend) currentCronRegistrationForClaim(
	ctx context.Context,
	claim *cronOccurrenceClaim,
) (*core.CronJob, bool, error) {
	occurrence, ok := parseIndexTimestamp(claim.OccurrenceAt)
	if !ok {
		return nil, false, fmt.Errorf("invalid claimed cron occurrence %q", claim.OccurrenceAt)
	}
	entry, err := b.cronStore.GetEntry(ctx, claim.CronName)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if claim.CronRevision != 0 && entry.Revision != claim.CronRevision {
		return nil, false, nil
	}
	cursor, ok := parseIndexTimestamp(entry.Cron.NextRunAt)
	if !ok {
		return nil, false, fmt.Errorf(
			"invalid cron cursor %q for %s",
			entry.Cron.NextRunAt,
			claim.CronName,
		)
	}
	if !entry.Cron.Enabled || !cursor.Equal(occurrence) {
		return nil, false, nil
	}
	return entry.Cron, true, nil
}

func (b *NATSBackend) cronCursorPastOccurrence(
	ctx context.Context,
	name string,
	occurrence time.Time,
) (bool, error) {
	entry, err := b.cronStore.GetEntry(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return false, nil
		}
		return false, err
	}
	cursor, ok := parseIndexTimestamp(entry.Cron.NextRunAt)
	if !ok {
		return false, fmt.Errorf("invalid cron cursor %q for %s", entry.Cron.NextRunAt, name)
	}
	return cursor.After(occurrence), nil
}

func (b *NATSBackend) cronRegistrationMatches(
	ctx context.Context,
	name string,
	occurrence time.Time,
	revision uint64,
) (bool, error) {
	entry, err := b.cronStore.GetEntry(ctx, name)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) || errors.Is(err, jetstream.ErrKeyDeleted) {
			return false, nil
		}
		return false, err
	}
	if revision != 0 && entry.Revision != revision {
		return false, nil
	}
	cursor, ok := parseIndexTimestamp(entry.Cron.NextRunAt)
	if !ok {
		return false, fmt.Errorf("invalid cron cursor %q for %s", entry.Cron.NextRunAt, name)
	}
	return entry.Cron.Enabled && cursor.Equal(occurrence), nil
}

func (b *NATSBackend) getCronClaim(ctx context.Context, key string) (cronOccurrenceClaim, uint64, error) {
	data, revision, err := b.cronClaims.Get(ctx, key)
	if err != nil {
		return cronOccurrenceClaim{}, 0, err
	}
	var claim cronOccurrenceClaim
	if err := json.Unmarshal(data, &claim); err != nil {
		return cronOccurrenceClaim{}, 0, err
	}
	return claim, revision, nil
}

func (b *NATSBackend) updateCronClaim(
	ctx context.Context,
	key string,
	claim *cronOccurrenceClaim,
	revision uint64,
) (uint64, error) {
	data, err := json.Marshal(claim)
	if err != nil {
		return 0, err
	}
	return b.cronClaims.Update(ctx, key, data, revision)
}

func (b *NATSBackend) maintainLegacyCronClaims(ctx context.Context, limit int) error {
	startRevision := b.cronLegacyCursor.Load()
	if startRevision == 0 {
		startRevision = 1
	}
	entries, nextRevision, exhausted, err := b.stats.EntriesFilteredFrom(
		ctx,
		cronClaimFilter,
		startRevision,
		limit,
	)
	if err != nil {
		return err
	}
	if exhausted {
		b.cronLegacyCursor.Store(1)
	} else {
		b.cronLegacyCursor.Store(nextRevision)
	}

	var firstErr error
	for _, entry := range entries {
		if err := b.maintainLegacyCronClaim(ctx, entry); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *NATSBackend) maintainLegacyCronClaim(ctx context.Context, entry kv.Entry) error {
	if entry.Operation == jetstream.KeyValueDelete || entry.Operation == jetstream.KeyValuePurge {
		return b.purgeLegacyCronClaimRevision(ctx, entry.Key, entry.Revision)
	}

	var claim cronOccurrenceClaim
	if err := json.Unmarshal(entry.Value, &claim); err != nil {
		return fmt.Errorf("decode legacy cron claim %s: %w", entry.Key, err)
	}
	safe, err := b.legacyCronClaimSafeToPurge(ctx, &claim)
	if err != nil {
		return err
	}
	if safe {
		return b.purgeLegacyCronClaimRevision(ctx, entry.Key, entry.Revision)
	}

	if _, err := b.cronClaims.Create(ctx, entry.Key, entry.Value); err != nil &&
		!errors.Is(err, jetstream.ErrKeyExists) {
		return fmt.Errorf("migrate legacy cron claim %s: %w", entry.Key, err)
	}
	if err := b.reconcileCronClaim(ctx, entry.Key); err != nil {
		return fmt.Errorf("reconcile migrated cron claim %s: %w", entry.Key, err)
	}

	safe, err = b.legacyCronClaimSafeToPurge(ctx, &claim)
	if err != nil {
		return err
	}
	if !safe {
		return nil
	}
	return b.purgeLegacyCronClaimRevision(ctx, entry.Key, entry.Revision)
}

func (b *NATSBackend) legacyCronClaimSafeToPurge(
	ctx context.Context,
	claim *cronOccurrenceClaim,
) (bool, error) {
	occurrence, ok := parseIndexTimestamp(claim.OccurrenceAt)
	if !ok {
		return false, fmt.Errorf("invalid legacy cron occurrence %q", claim.OccurrenceAt)
	}
	advanced, err := b.cronCursorPastOccurrence(ctx, claim.CronName, occurrence)
	if err == nil && advanced {
		return true, nil
	}
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) &&
		!errors.Is(err, jetstream.ErrKeyDeleted) {
		return false, err
	}

	current, err := b.cronRegistrationMatches(
		ctx,
		claim.CronName,
		occurrence,
		claim.CronRevision,
	)
	if err != nil {
		return false, err
	}
	if current {
		return false, nil
	}
	if claim.Status != "pending" || claim.JobID == "" {
		return true, nil
	}
	_, err = b.Info(ctx, claim.JobID)
	return err == nil, nil
}

func (b *NATSBackend) purgeLegacyCronClaimRevision(
	ctx context.Context,
	key string,
	revision uint64,
) error {
	stream, err := b.js.Stream(ctx, "KV_"+BucketStats)
	if err != nil {
		return fmt.Errorf("open stats KV stream: %w", err)
	}
	subject := fmt.Sprintf("$KV.%s.%s", BucketStats, key)
	if err := stream.Purge(
		ctx,
		jetstream.WithPurgeSubject(subject),
		jetstream.WithPurgeSequence(revision+1),
	); err != nil {
		return fmt.Errorf("purge legacy cron claim %s through revision %d: %w", key, revision, err)
	}
	return nil
}

func cronOccurrenceKey(name string, occurrence time.Time) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s%x.%d", cronClaimPrefix, sum[:8], occurrence.UnixMilli())
}

func parseCronSchedule(expression, timezone string) (cron.Schedule, string, error) {
	expression = strings.TrimSpace(expression)
	if expression == "" {
		return nil, "", fmt.Errorf("cron expression is required")
	}
	embeddedTimezone, bareExpression := splitCronTimezone(expression)
	effectiveTimezone := timezone
	if effectiveTimezone == "" {
		effectiveTimezone = embeddedTimezone
	}
	if effectiveTimezone == "" {
		effectiveTimezone = "UTC"
	}
	location, err := time.LoadLocation(effectiveTimezone)
	if err != nil {
		return nil, "", fmt.Errorf("invalid timezone %q: %w", effectiveTimezone, err)
	}
	schedule, err := cronParser.Parse("CRON_TZ=" + location.String() + " " + bareExpression)
	if err != nil {
		return nil, "", fmt.Errorf("invalid expression %q: %w", bareExpression, err)
	}
	return schedule, location.String(), nil
}

func splitCronTimezone(expression string) (string, string) {
	fields := strings.Fields(expression)
	if len(fields) > 1 &&
		(strings.HasPrefix(fields[0], "CRON_TZ=") || strings.HasPrefix(fields[0], "TZ=")) {
		parts := strings.SplitN(fields[0], "=", 2)
		return parts[1], strings.Join(fields[1:], " ")
	}
	return "", expression
}

func normalizeCronTemplate(cronJob *core.CronJob) {
	if cronJob.JobTemplate == nil && cronJob.JobType != "" {
		cronJob.JobTemplate = &core.CronJobTemplate{
			Type: cronJob.JobType,
			Args: cronJob.Args,
		}
	}
	if cronJob.JobTemplate == nil {
		return
	}
	if cronJob.JobTemplate.Options == nil {
		cronJob.JobTemplate.Options = &core.EnqueueOptions{}
	}
	if cronJob.JobTemplate.Options.Queue == "" {
		if cronJob.Queue != "" {
			cronJob.JobTemplate.Options.Queue = cronJob.Queue
		} else {
			cronJob.JobTemplate.Options.Queue = "default"
		}
	}
}

func cronRegistrationToJob(registration *core.CronJob, jobID string) *core.Job {
	visibilityTimeout := 600000
	job := &core.Job{
		ID:                  jobID,
		Queue:               "default",
		VisibilityTimeoutMs: &visibilityTimeout,
	}
	if registration == nil || registration.JobTemplate == nil {
		return job
	}
	job.Type = registration.JobTemplate.Type
	job.Args = registration.JobTemplate.Args
	opts := registration.JobTemplate.Options
	if opts == nil {
		return job
	}
	if opts.Queue != "" {
		job.Queue = opts.Queue
	}
	job.Priority = opts.Priority
	job.TimeoutMs = opts.TimeoutMs
	job.ScheduledAt = opts.ScheduledAt
	if job.ScheduledAt == "" {
		job.ScheduledAt = opts.DelayUntil
	}
	job.ExpiresAt = opts.ExpiresAt
	job.Retry = opts.Retry
	if job.Retry == nil {
		job.Retry = opts.RetryPolicy
	}
	if job.Retry != nil {
		job.MaxAttempts = &job.Retry.MaxAttempts
	}
	job.Unique = opts.Unique
	job.Tags = append([]string(nil), opts.Tags...)
	if opts.VisibilityTimeoutMs != nil {
		job.VisibilityTimeoutMs = opts.VisibilityTimeoutMs
	}
	job.Meta = append(json.RawMessage(nil), opts.Metadata...)
	job.RateLimit = opts.RateLimit
	return job
}

func (b *NATSBackend) validateCronPayloads(cronJob *core.CronJob) error {
	registrationData, err := json.Marshal(cronJob)
	if err != nil {
		return core.NewInvalidRequestError(
			fmt.Sprintf("Cron registration cannot be serialized: %v", err),
			map[string]any{"field": "cron", "validation": "serializable"},
		)
	}
	if err := b.validateCronPayloadSize("registration", registrationData, 0); err != nil {
		return err
	}

	now := time.Now()
	job := cronRegistrationToJob(cronJob, core.NewUUIDv7())
	prepareGeneratedCronJob(job, now)
	jobData, err := marshalJobState(job)
	if err != nil {
		return core.NewInvalidRequestError(
			fmt.Sprintf("Cron generated job cannot be serialized: %v", err),
			map[string]any{"field": "job_template", "validation": "serializable"},
		)
	}
	if err := b.validateCronPayloadSize(
		"generated_job",
		jobData,
		cronKVHeaderSafetyMargin,
	); err != nil {
		return err
	}

	claimData, err := json.Marshal(cronOccurrenceClaim{
		CronName:     cronJob.Name,
		OccurrenceAt: core.FormatTime(now),
		JobID:        core.NewUUIDv7(),
		Status:       "pending",
		Phase:        "preparing",
		Owner:        core.NewUUIDv7(),
		LeaseUntil:   core.FormatTime(now.Add(cronClaimLease)),
		ClaimedAt:    core.FormatTime(now),
		CronRevision: ^uint64(0),
	})
	if err != nil {
		return core.NewInvalidRequestError(
			fmt.Sprintf("Cron occurrence claim cannot be serialized: %v", err),
			map[string]any{"field": "name", "validation": "serializable"},
		)
	}
	if err := b.validateCronPayloadSize(
		"occurrence_claim",
		claimData,
		cronKVHeaderSafetyMargin,
	); err != nil {
		return err
	}

	updatedRegistration := *cronJob
	updatedRegistration.LastRunAt = core.FormatTime(now)
	updatedRegistrationData, err := json.Marshal(&updatedRegistration)
	if err != nil {
		return core.NewInvalidRequestError(
			fmt.Sprintf("Cron registration cannot be serialized for cursor updates: %v", err),
			map[string]any{"field": "cron", "validation": "serializable"},
		)
	}
	return b.validateCronPayloadSize(
		"registration_update",
		updatedRegistrationData,
		cronKVHeaderSafetyMargin,
	)
}

func (b *NATSBackend) validateCronPayloadSize(
	representation string,
	data []byte,
	safetyMargin int64,
) error {
	maxPayload := b.nc.MaxPayload()
	limit := maxPayload - safetyMargin
	if limit >= 0 && int64(len(data)) <= limit {
		return nil
	}
	return core.NewInvalidRequestError(
		fmt.Sprintf(
			"Cron %s exceeds the connected NATS server payload limit.",
			strings.ReplaceAll(representation, "_", " "),
		),
		map[string]any{
			"field":             "job_template",
			"validation":        "max_payload",
			"representation":    representation,
			"serialized_bytes":  len(data),
			"max_payload_bytes": maxPayload,
			"safety_margin":     safetyMargin,
		},
	)
}

func prepareGeneratedCronJob(job *core.Job, now time.Time) {
	if job.Queue == "" {
		job.Queue = "default"
	}
	job.CreatedAt = core.FormatTime(now)
	job.Attempt = 0
	if job.ScheduledAt != "" {
		scheduledTime, err := time.Parse(time.RFC3339, job.ScheduledAt)
		if err == nil && scheduledTime.After(now) {
			job.State = core.StateScheduled
			job.EnqueuedAt = core.FormatTime(now)
			return
		}
	}
	job.State = core.StateAvailable
	job.EnqueuedAt = core.FormatTime(now)
}

func (b *NATSBackend) isCronInstanceRunning(ctx context.Context, cronName string) bool {
	key := "cron-instance." + cronName
	data, _, err := b.stats.Get(ctx, key)
	if err != nil {
		return false
	}
	jobID := string(data)
	job, err := b.getJobState(ctx, jobID)
	if err != nil {
		return false
	}
	return !core.IsTerminalState(job.State)
}

func (b *NATSBackend) setCronInstance(ctx context.Context, cronName, jobID string) {
	key := "cron-instance." + cronName
	_, _ = b.stats.Put(ctx, key, []byte(jobID))
}
