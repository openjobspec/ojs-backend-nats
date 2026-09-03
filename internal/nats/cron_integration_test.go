package nats

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/openjobspec/ojs-backend-nats/internal/core"
)

func TestParseCronSchedule_AppliesTimezoneAndDefaultsUTC(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)

	newYork, timezone, err := parseCronSchedule("0 9 * * *", "America/New_York")
	if err != nil {
		t.Fatalf("parseCronSchedule(New York) error = %v", err)
	}
	if timezone != "America/New_York" {
		t.Fatalf("timezone = %q, want America/New_York", timezone)
	}
	if got, want := newYork.Next(now), time.Date(2026, 8, 11, 13, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("New York next = %v, want %v", got, want)
	}

	utc, timezone, err := parseCronSchedule("0 9 * * *", "")
	if err != nil {
		t.Fatalf("parseCronSchedule(UTC default) error = %v", err)
	}
	if timezone != "UTC" {
		t.Fatalf("default timezone = %q, want UTC", timezone)
	}
	if got, want := utc.Next(now), time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("UTC next = %v, want %v", got, want)
	}

	tokyo, timezone, err := parseCronSchedule("CRON_TZ=Asia/Tokyo 0 9 * * *", "")
	if err != nil {
		t.Fatalf("parseCronSchedule(embedded timezone) error = %v", err)
	}
	if timezone != "Asia/Tokyo" {
		t.Fatalf("embedded timezone = %q, want Asia/Tokyo", timezone)
	}
	if got, want := tokyo.Next(now), time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("Tokyo next = %v, want %v", got, want)
	}
}

func TestCronOccurrences_TwoSchedulersEnqueueOnce(t *testing.T) {
	backend1 := newIntegrationBackend(t)
	backend2 := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-distributed-" + core.NewUUIDv7()
	name := "cron-distributed-" + core.NewUUIDv7()
	due := registerDueCron(t, backend1, name, queue)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, backend := range []*NATSBackend{backend1, backend2} {
		backend := backend
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- backend.FireCronJobs(ctx)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("FireCronJobs() error = %v", err)
		}
	}

	if claims := cronClaimsFor(t, backend1, name); len(claims) != 0 {
		t.Fatalf("completed cron occurrence claims retained = %+v", claims)
	}

	entry, err := backend1.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() error = %v", err)
	}
	next, ok := parseIndexTimestamp(entry.NextRunAt)
	if !ok || !next.After(due) {
		t.Fatalf("cursor did not advance: due=%v next=%q", due, entry.NextRunAt)
	}

	fetched := fetchJobEventually(t, backend1, queue, "cron-worker")
	if _, err := backend1.Ack(ctx, fetched.ID, nil); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
	if jobs, err := backend1.Fetch(ctx, []string{queue}, 1, "cron-worker", 5000); err != nil || len(jobs) != 0 {
		t.Fatalf("duplicate cron delivery after ACK: jobs=%d err=%v", len(jobs), err)
	}
}

func TestRegisterCron_RejectsGeneratedJobAboveServerMaxPayload(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	name := "p-" + core.NewUUIDv7()[:8]
	now := time.Now()
	newCron := func(args json.RawMessage) *core.CronJob {
		return &core.CronJob{
			Name:          name,
			Expression:    "* * * * *",
			Timezone:      "UTC",
			OverlapPolicy: "allow",
			Enabled:       true,
			CreatedAt:     core.FormatTime(now),
			NextRunAt:     core.FormatTime(now.Add(time.Minute)),
			JobTemplate: &core.CronJobTemplate{
				Type: "cron.payload.limit",
				Args: args,
				Options: &core.EnqueueOptions{
					Queue: "q",
				},
			},
		}
	}

	base := newCron(json.RawMessage(`[""]`))
	baseRegistration, err := json.Marshal(base)
	if err != nil {
		t.Fatalf("marshal base registration: %v", err)
	}
	maxPayload := backend.nc.MaxPayload()
	argBytes := int(maxPayload) - 1 - len(baseRegistration)
	if argBytes <= 0 {
		t.Fatalf("NATS max payload %d is too small for the cron registration fixture", maxPayload)
	}
	cronJob := newCron(json.RawMessage(`["` + strings.Repeat("x", argBytes) + `"]`))
	registrationData, err := json.Marshal(cronJob)
	if err != nil {
		t.Fatalf("marshal registration: %v", err)
	}
	if got, want := int64(len(registrationData)), maxPayload-1; got != want {
		t.Fatalf("registration size = %d, want %d", got, want)
	}

	job := cronRegistrationToJob(cronJob, core.NewUUIDv7())
	prepareGeneratedCronJob(job, now)
	jobData, err := marshalJobState(job)
	if err != nil {
		t.Fatalf("marshal generated job: %v", err)
	}
	if int64(len(jobData))+cronKVHeaderSafetyMargin <= maxPayload {
		t.Fatalf(
			"generated job plus KV header margin = %d, want > max payload %d",
			int64(len(jobData))+cronKVHeaderSafetyMargin,
			maxPayload,
		)
	}

	_, err = backend.RegisterCron(ctx, cronJob)
	var ojsErr *core.OJSError
	if !errors.As(err, &ojsErr) || ojsErr.Code != core.ErrCodeInvalidRequest {
		t.Fatalf("RegisterCron() error = %v, want typed invalid_request", err)
	}
	if got := ojsErr.Details["representation"]; got != "generated_job" {
		t.Fatalf("error representation = %v, want generated_job", got)
	}
	if _, getErr := backend.cronStore.Get(ctx, name); getErr == nil {
		t.Fatal("oversized cron registration was persisted")
	}
}

func TestCronOccurrence_LargeTemplateClaimsFiresAndAdvances(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-large-" + core.NewUUIDv7()
	name := "cron-large-" + core.NewUUIDv7()
	argBytes := 1 << 20
	if maxPayload := backend.nc.MaxPayload(); int64(argBytes*2) > maxPayload {
		argBytes = int(maxPayload / 2)
	}
	args, err := json.Marshal([]string{strings.Repeat("x", argBytes)})
	if err != nil {
		t.Fatalf("marshal large args: %v", err)
	}
	priority := 7
	timeout := 45000
	visibilityTimeout := 90000
	template := &core.CronJobTemplate{
		Type: "cron.large",
		Args: args,
		Options: &core.EnqueueOptions{
			Queue:               queue,
			Priority:            &priority,
			TimeoutMs:           &timeout,
			Retry:               &core.RetryPolicy{MaxAttempts: 5, InitialInterval: "PT1S"},
			Tags:                []string{"large", "cron"},
			VisibilityTimeoutMs: &visibilityTimeout,
			Metadata:            json.RawMessage(`{"source":"large-template"}`),
			RateLimit:           &core.RateLimitPolicy{MaxPerSecond: 100},
		},
	}
	serializedTemplate, err := json.Marshal(template)
	if err != nil {
		t.Fatalf("marshal large template: %v", err)
	}
	if len(serializedTemplate) <= argBytes {
		t.Fatalf("serialized template size = %d, want > args size %d", len(serializedTemplate), argBytes)
	}

	_, err = backend.RegisterCron(ctx, &core.CronJob{
		Name:        name,
		Expression:  "* * * * *",
		Timezone:    "UTC",
		JobTemplate: template,
	})
	if err != nil {
		t.Fatalf("RegisterCron() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = backend.DeleteCron(context.Background(), name)
	})

	entry, err := backend.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() error = %v", err)
	}
	due := time.Now().UTC().Truncate(time.Minute)
	entry.NextRunAt = core.FormatTime(due)
	if err := backend.cronStore.Register(ctx, entry); err != nil {
		t.Fatalf("cronStore.Register(due) error = %v", err)
	}

	originalPublish := backend.publishJob
	publishStarted := make(chan struct{})
	continuePublish := make(chan struct{})
	var publishOnce sync.Once
	var releaseOnce sync.Once
	releasePublish := func() {
		releaseOnce.Do(func() { close(continuePublish) })
	}
	defer releasePublish()
	backend.publishJob = func(ctx context.Context, publishQueue, jobID string, seq uint64) error {
		publishOnce.Do(func() { close(publishStarted) })
		select {
		case <-continuePublish:
		case <-ctx.Done():
			return ctx.Err()
		}
		return originalPublish(ctx, publishQueue, jobID, seq)
	}

	fireErr := make(chan error, 1)
	go func() {
		fireErr <- backend.FireCronJobs(ctx)
	}()
	select {
	case <-publishStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("large cron occurrence did not reach publication")
	}

	var claims []cronOccurrenceClaim
	deadline := time.Now().Add(2 * time.Second)
	for len(claims) == 0 && time.Now().Before(deadline) {
		claims = cronClaimsFor(t, backend, name)
		if len(claims) == 0 {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(claims) != 1 {
		t.Fatalf("large cron claims = %d, want 1", len(claims))
	}
	serializedClaim, err := json.Marshal(claims[0])
	if err != nil {
		t.Fatalf("marshal large claim: %v", err)
	}
	if len(serializedClaim) > 512 {
		t.Fatalf("serialized claim size = %d, want bounded identity-only claim", len(serializedClaim))
	}

	releasePublish()
	select {
	case err := <-fireErr:
		if err != nil {
			t.Fatalf("FireCronJobs() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("FireCronJobs() did not complete")
	}
	backend.publishJob = originalPublish

	if claims := cronClaimsFor(t, backend, name); len(claims) != 0 {
		t.Fatalf("completed large cron claims retained = %+v", claims)
	}
	advanced, err := backend.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() after fire error = %v", err)
	}
	next, ok := parseIndexTimestamp(advanced.NextRunAt)
	if !ok || !next.After(due) {
		t.Fatalf("large cron cursor did not advance: due=%v next=%q", due, advanced.NextRunAt)
	}

	fetched := fetchJobEventually(t, backend, queue, "cron-large-worker")
	if !bytes.Equal(fetched.Args, args) {
		t.Fatalf("fetched large args differ: got=%d bytes want=%d bytes", len(fetched.Args), len(args))
	}
	if fetched.Priority == nil || *fetched.Priority != priority {
		t.Fatalf("fetched priority = %v, want %d", fetched.Priority, priority)
	}
	if fetched.TimeoutMs == nil || *fetched.TimeoutMs != timeout {
		t.Fatalf("fetched timeout = %v, want %d", fetched.TimeoutMs, timeout)
	}
	if fetched.VisibilityTimeoutMs == nil || *fetched.VisibilityTimeoutMs != visibilityTimeout {
		t.Fatalf(
			"fetched visibility timeout = %v, want %d",
			fetched.VisibilityTimeoutMs,
			visibilityTimeout,
		)
	}
	if fetched.Retry == nil || fetched.Retry.MaxAttempts != 5 {
		t.Fatalf("fetched retry = %+v, want max_attempts 5", fetched.Retry)
	}
	if got, want := strings.Join(fetched.Tags, ","), "large,cron"; got != want {
		t.Fatalf("fetched tags = %q, want %q", got, want)
	}
	if !bytes.Equal(fetched.Meta, template.Options.Metadata) {
		t.Fatalf("fetched metadata = %s, want %s", fetched.Meta, template.Options.Metadata)
	}
	if fetched.RateLimit == nil || fetched.RateLimit.MaxPerSecond != 100 {
		t.Fatalf("fetched rate limit = %+v, want 100", fetched.RateLimit)
	}
	if _, err := backend.Ack(ctx, fetched.ID, nil); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
}

func TestCronOccurrence_AmbiguousPublishRecoversAfterRestart(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-ambiguous-" + core.NewUUIDv7()
	name := "cron-ambiguous-" + core.NewUUIDv7()
	registerDueCron(t, backend, name, queue)

	originalPublish := backend.publishJob
	backend.publishJob = func(ctx context.Context, publishQueue, jobID string, seq uint64) error {
		if publishQueue != queue {
			return originalPublish(ctx, publishQueue, jobID, seq)
		}
		if err := originalPublish(ctx, publishQueue, jobID, seq); err != nil {
			return err
		}
		return context.DeadlineExceeded
	}
	if err := backend.FireCronJobs(ctx); err == nil {
		t.Fatal("FireCronJobs() error = nil, want ambiguous publish error")
	}
	claims := cronClaimsFor(t, backend, name)
	if len(claims) != 1 || claims[0].Status == "completed" {
		t.Fatalf("claim after ambiguous publish = %+v", claims)
	}
	claimedJobID := claims[0].JobID

	restarted := newIntegrationBackend(t)
	time.Sleep(cronClaimLease + 100*time.Millisecond)
	if err := restarted.FireCronJobs(ctx); err != nil {
		t.Fatalf("FireCronJobs() after restart error = %v", err)
	}
	claims = cronClaimsFor(t, restarted, name)
	if len(claims) != 0 {
		t.Fatalf("completed claim retained after restart = %+v", claims)
	}
	fetched := fetchJobEventually(t, restarted, queue, "cron-restart-worker")
	if fetched.ID != claimedJobID {
		t.Fatalf("fetched ID = %q, want %q", fetched.ID, claimedJobID)
	}

	// Re-running after cursor advancement must not create another instance for
	// the same occurrence.
	if err := restarted.FireCronJobs(ctx); err != nil {
		t.Fatalf("second FireCronJobs() error = %v", err)
	}
	if claims := cronClaimsFor(t, restarted, name); len(claims) != 0 {
		t.Fatalf("second reconciliation retained claims = %+v", claims)
	}
}

func TestCronOccurrences_MultipleFiresLeaveNoCompletedClaims(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-retention-" + core.NewUUIDv7()
	name := "cron-retention-" + core.NewUUIDv7()
	registerDueCron(t, backend, name, queue)

	entry, err := backend.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() error = %v", err)
	}
	firstOccurrence := time.Now().UTC().Truncate(time.Minute).Add(-2 * time.Minute)
	entry.NextRunAt = core.FormatTime(firstOccurrence)
	if err := backend.cronStore.Register(ctx, entry); err != nil {
		t.Fatalf("cronStore.Register(catch-up) error = %v", err)
	}

	if err := backend.FireCronJobs(ctx); err != nil {
		t.Fatalf("FireCronJobs() error = %v", err)
	}
	if err := backend.reconcileCronClaims(ctx); err != nil {
		t.Fatalf("reconcileCronClaims() error = %v", err)
	}
	if claims := cronClaimsFor(t, backend, name); len(claims) != 0 {
		t.Fatalf("completed claims retained after catch-up = %+v", claims)
	}

	updated, err := backend.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() after catch-up error = %v", err)
	}
	next, ok := parseIndexTimestamp(updated.NextRunAt)
	if !ok || !next.After(time.Now()) {
		t.Fatalf("cron cursor = %q, want future occurrence", updated.NextRunAt)
	}

	seen := make(map[string]struct{}, 3)
	for i := 0; i < 3; i++ {
		job := fetchJobEventually(t, backend, queue, "cron-retention-worker")
		seen[job.ID] = struct{}{}
		if _, err := backend.Ack(ctx, job.ID, nil); err != nil {
			t.Fatalf("Ack(%s) error = %v", job.ID, err)
		}
	}
	if len(seen) != 3 {
		t.Fatalf("unique cron jobs = %d, want 3", len(seen))
	}
}

func TestReconcileCronClaim_StaleRevisionCannotDeleteNewerClaim(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	name := "cron-stale-delete-" + core.NewUUIDv7()
	occurrence := time.Now().UTC().Add(-time.Minute).Truncate(time.Millisecond)
	key := cronOccurrenceKey(name, occurrence)

	oldClaim := cronOccurrenceClaim{
		CronName:     name,
		OccurrenceAt: core.FormatTime(occurrence),
		Status:       "completed",
		Owner:        "stale-owner",
		LeaseUntil:   core.FormatTime(time.Now()),
		ClaimedAt:    core.FormatTime(time.Now().Add(-time.Minute)),
	}
	oldData, err := json.Marshal(oldClaim)
	if err != nil {
		t.Fatalf("json.Marshal(old claim) error = %v", err)
	}
	oldRevision, err := backend.cronClaims.Create(ctx, key, oldData)
	if err != nil {
		t.Fatalf("cronClaims.Create(old claim) error = %v", err)
	}
	t.Cleanup(func() {
		if _, revision, getErr := backend.cronClaims.Get(context.Background(), key); getErr == nil {
			_ = backend.cronClaims.DeleteRevision(context.Background(), key, revision)
		}
	})

	newClaim := oldClaim
	newClaim.Status = "pending"
	newClaim.Owner = "new-owner"
	newClaim.JobID = core.NewUUIDv7()
	newClaim.LeaseUntil = core.FormatTime(time.Now().Add(time.Hour))
	newData, err := json.Marshal(newClaim)
	if err != nil {
		t.Fatalf("json.Marshal(new claim) error = %v", err)
	}
	newRevision, err := backend.cronClaims.Update(ctx, key, newData, oldRevision)
	if err != nil {
		t.Fatalf("cronClaims.Update(new claim) error = %v", err)
	}

	if err := backend.cronClaims.DeleteRevision(ctx, key, oldRevision); !errors.Is(err, jetstream.ErrKeyExists) {
		t.Fatalf("stale DeleteRevision() error = %v, want ErrKeyExists", err)
	}
	got, revision, err := backend.getCronClaim(ctx, key)
	if err != nil {
		t.Fatalf("getCronClaim() after stale delete error = %v", err)
	}
	if revision != newRevision || got.Owner != newClaim.Owner || got.JobID != newClaim.JobID {
		t.Fatalf("newer claim changed: revision=%d claim=%+v", revision, got)
	}
}

func TestCronClaimBucket_ExpiresValuesAndDeleteMarkers(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	bucket := "ojs-cron-claims-test-" + core.NewUUIDv7()
	const ttl = 500 * time.Millisecond

	claims, err := createOrUpdateCronClaimBucket(ctx, backend.js, bucket, ttl)
	if err != nil {
		t.Fatalf("createOrUpdateCronClaimBucket() error = %v", err)
	}
	t.Cleanup(func() {
		_ = backend.js.DeleteKeyValue(context.Background(), bucket)
	})
	status, err := claims.Status(ctx)
	if err != nil {
		t.Fatalf("claims.Status() error = %v", err)
	}
	if status.History() != 1 || status.TTL() != ttl {
		t.Fatalf("claim bucket history=%d ttl=%v, want history=1 ttl=%v", status.History(), status.TTL(), ttl)
	}

	valueKey := "value"
	if _, err := claims.Put(ctx, valueKey, []byte("claim")); err != nil {
		t.Fatalf("claims.Put(value) error = %v", err)
	}
	waitForKVHistoryGone(t, claims, valueKey, 5*time.Second)

	tombstoneKey := "tombstone"
	revision, err := claims.Put(ctx, tombstoneKey, []byte("claim"))
	if err != nil {
		t.Fatalf("claims.Put(tombstone) error = %v", err)
	}
	if err := claims.Delete(ctx, tombstoneKey, jetstream.LastRevision(revision)); err != nil {
		t.Fatalf("claims.Delete(tombstone) error = %v", err)
	}
	history, err := claims.History(ctx, tombstoneKey)
	if err != nil || len(history) != 1 || history[0].Operation() != jetstream.KeyValueDelete {
		t.Fatalf("deleted claim history = %+v, err=%v", history, err)
	}
	waitForKVHistoryGone(t, claims, tombstoneKey, 5*time.Second)
}

func TestCronClaimBucket_InFlightClaimSurvivesLeaseWindow(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	status, err := backend.js.KeyValue(ctx, bucketCronClaims)
	if err != nil {
		t.Fatalf("open cron claim bucket error = %v", err)
	}
	bucketStatus, err := status.Status(ctx)
	if err != nil {
		t.Fatalf("cron claim bucket status error = %v", err)
	}
	if bucketStatus.History() != 1 || bucketStatus.TTL() <= cronClaimLease {
		t.Fatalf("claim bucket history=%d ttl=%v, want history=1 and ttl>%v", bucketStatus.History(), bucketStatus.TTL(), cronClaimLease)
	}
	stats, err := backend.js.KeyValue(ctx, BucketStats)
	if err != nil {
		t.Fatalf("open stats bucket error = %v", err)
	}
	statsStatus, err := stats.Status(ctx)
	if err != nil {
		t.Fatalf("stats bucket status error = %v", err)
	}
	if statsStatus.TTL() != 0 {
		t.Fatalf("stats bucket TTL = %v, want no global TTL", statsStatus.TTL())
	}

	name := "cron-lease-survival-" + core.NewUUIDv7()
	occurrence := time.Now().UTC().Truncate(time.Millisecond)
	key := cronOccurrenceKey(name, occurrence)
	claim := cronOccurrenceClaim{
		CronName:     name,
		OccurrenceAt: core.FormatTime(occurrence),
		Status:       "pending",
		Owner:        "live-owner",
		LeaseUntil:   core.FormatTime(time.Now().Add(cronClaimLease)),
		ClaimedAt:    core.NowFormatted(),
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("json.Marshal(claim) error = %v", err)
	}
	revision, err := backend.cronClaims.Create(ctx, key, data)
	if err != nil {
		t.Fatalf("cronClaims.Create() error = %v", err)
	}
	t.Cleanup(func() {
		_ = backend.cronClaims.DeleteRevision(context.Background(), key, revision)
	})

	time.Sleep(cronClaimLease + 100*time.Millisecond)
	if !backend.cronClaims.Exists(ctx, key) {
		t.Fatal("in-flight claim expired at the lease/recovery boundary")
	}
}

func TestLegacyCronClaimMaintenance_RemovesTombstone(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	key := cronClaimPrefix + "legacy-tombstone-" + core.NewUUIDv7()
	revision, err := backend.stats.Create(ctx, key, []byte(`{"legacy":true}`))
	if err != nil {
		t.Fatalf("stats.Create() error = %v", err)
	}
	if err := backend.stats.DeleteRevision(ctx, key, revision); err != nil {
		t.Fatalf("stats.DeleteRevision() error = %v", err)
	}

	entries, err := backend.stats.EntriesFiltered(ctx, key)
	if err != nil || len(entries) != 1 || entries[0].Operation != jetstream.KeyValueDelete {
		t.Fatalf("legacy tombstone entries = %+v, err=%v", entries, err)
	}
	if err := backend.maintainLegacyCronClaim(ctx, entries[0]); err != nil {
		t.Fatalf("maintainLegacyCronClaim() error = %v", err)
	}
	entries, err = backend.stats.EntriesFiltered(ctx, key)
	if err != nil || len(entries) != 0 {
		t.Fatalf("legacy tombstone remained after maintenance: entries=%+v err=%v", entries, err)
	}
}

func TestLegacyCronClaimPurge_PreservesNewerRevision(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	key := cronClaimPrefix + "legacy-fence-" + core.NewUUIDv7()
	oldRevision, err := backend.stats.Create(ctx, key, []byte("old"))
	if err != nil {
		t.Fatalf("stats.Create(old) error = %v", err)
	}
	newRevision, err := backend.stats.Update(ctx, key, []byte("new"), oldRevision)
	if err != nil {
		t.Fatalf("stats.Update(new) error = %v", err)
	}
	t.Cleanup(func() {
		_ = backend.purgeLegacyCronClaimRevision(context.Background(), key, newRevision)
	})

	if err := backend.purgeLegacyCronClaimRevision(ctx, key, oldRevision); err != nil {
		t.Fatalf("purgeLegacyCronClaimRevision(old) error = %v", err)
	}
	value, revision, err := backend.stats.Get(ctx, key)
	if err != nil {
		t.Fatalf("stats.Get(new) error = %v", err)
	}
	if revision != newRevision || string(value) != "new" {
		t.Fatalf("newer revision changed: revision=%d value=%q", revision, value)
	}
}

func TestLegacyCronClaimMaintenance_MigratesInFlightSafely(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-legacy-migration-" + core.NewUUIDv7()
	name := "cron-legacy-migration-" + core.NewUUIDv7()
	occurrence := registerDueCron(t, backend, name, queue)
	registration, err := backend.cronStore.GetEntry(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.GetEntry() error = %v", err)
	}
	jobID := core.NewUUIDv7()
	claim := cronOccurrenceClaim{
		CronName:     name,
		OccurrenceAt: core.FormatTime(occurrence),
		JobID:        jobID,
		Status:       "pending",
		Owner:        "legacy-replica",
		LeaseUntil:   core.FormatTime(time.Now().Add(150 * time.Millisecond)),
		ClaimedAt:    core.NowFormatted(),
		CronRevision: registration.Revision,
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("json.Marshal(claim) error = %v", err)
	}
	key := cronOccurrenceKey(name, occurrence)
	legacyRevision, err := backend.stats.Create(ctx, key, data)
	if err != nil {
		t.Fatalf("stats.Create(legacy claim) error = %v", err)
	}
	t.Cleanup(func() {
		_ = backend.purgeLegacyCronClaimRevision(context.Background(), key, legacyRevision)
	})

	entries, err := backend.stats.EntriesFiltered(ctx, key)
	if err != nil || len(entries) != 1 {
		t.Fatalf("legacy claim entries = %+v, err=%v", entries, err)
	}
	if err := backend.maintainLegacyCronClaim(ctx, entries[0]); err != nil {
		t.Fatalf("maintainLegacyCronClaim(active lease) error = %v", err)
	}
	if !backend.stats.Exists(ctx, key) || !backend.cronClaims.Exists(ctx, key) {
		t.Fatal("active legacy claim was not retained in stats and copied to the claim bucket")
	}
	if _, err := backend.Info(ctx, jobID); err == nil {
		t.Fatal("active legacy claim was reconciled before its lease expired")
	}

	time.Sleep(200 * time.Millisecond)
	entries, err = backend.stats.EntriesFiltered(ctx, key)
	if err != nil || len(entries) != 1 {
		t.Fatalf("legacy claim entries after lease = %+v, err=%v", entries, err)
	}
	if err := backend.maintainLegacyCronClaim(ctx, entries[0]); err != nil {
		t.Fatalf("maintainLegacyCronClaim(expired lease) error = %v", err)
	}
	if backend.stats.Exists(ctx, key) || backend.cronClaims.Exists(ctx, key) {
		t.Fatal("reconciled legacy claim value remained after cursor advancement")
	}
	entries, err = backend.stats.EntriesFiltered(ctx, key)
	if err != nil || len(entries) != 0 {
		t.Fatalf("legacy claim history remained: entries=%+v err=%v", entries, err)
	}
	if _, err := backend.Info(ctx, jobID); err != nil {
		t.Fatalf("legacy claim was purged before its job was confirmed: %v", err)
	}
}

func TestReconcileCronClaim_RemovesCompletedClaimPastCursor(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-completed-cleanup-" + core.NewUUIDv7()
	name := "cron-completed-cleanup-" + core.NewUUIDv7()
	occurrence := registerDueCron(t, backend, name, queue)

	entry, err := backend.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() error = %v", err)
	}

	entry.NextRunAt = core.FormatTime(occurrence.Add(time.Minute))
	if err := backend.cronStore.Register(ctx, entry); err != nil {
		t.Fatalf("cronStore.Register(advanced cursor) error = %v", err)
	}

	claim := cronOccurrenceClaim{
		CronName:     name,
		OccurrenceAt: core.FormatTime(occurrence),
		JobID:        core.NewUUIDv7(),
		Status:       "completed",
		Owner:        "stopped-replica",
		LeaseUntil:   core.FormatTime(time.Now().Add(-time.Minute)),
		ClaimedAt:    core.FormatTime(time.Now().Add(-2 * time.Minute)),
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("json.Marshal(claim) error = %v", err)
	}
	key := cronOccurrenceKey(name, occurrence)
	if _, err := backend.cronClaims.Create(ctx, key, data); err != nil {
		t.Fatalf("cronClaims.Create(completed claim) error = %v", err)
	}

	if err := backend.reconcileCronClaims(ctx); err != nil {
		t.Fatalf("reconcileCronClaims() error = %v", err)
	}
	if backend.cronClaims.Exists(ctx, key) {
		t.Fatal("completed claim remained after cursor advanced past its occurrence")
	}
}

func TestDeleteCron_WaitsForInFlightClaimThenCleansIt(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-delete-claim-" + core.NewUUIDv7()
	name := "cron-delete-claim-" + core.NewUUIDv7()
	occurrence := registerDueCron(t, backend, name, queue)
	registration, err := backend.cronStore.GetEntry(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.GetEntry() error = %v", err)
	}
	jobID := core.NewUUIDv7()
	claim := cronOccurrenceClaim{
		CronName:     name,
		OccurrenceAt: core.FormatTime(occurrence),
		JobID:        jobID,
		Status:       "pending",
		Owner:        "other-replica",
		LeaseUntil:   core.FormatTime(time.Now().Add(150 * time.Millisecond)),
		ClaimedAt:    core.NowFormatted(),
		CronRevision: registration.Revision,
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("json.Marshal(claim) error = %v", err)
	}
	key := cronOccurrenceKey(name, occurrence)
	if _, err := backend.cronClaims.Create(ctx, key, data); err != nil {
		t.Fatalf("cronClaims.Create(claim) error = %v", err)
	}

	result := make(chan error, 1)
	go func() {
		_, deleteErr := backend.DeleteCron(ctx, name)
		result <- deleteErr
	}()

	time.Sleep(50 * time.Millisecond)
	if !backend.cronClaims.Exists(ctx, key) {
		t.Fatal("DeleteCron removed an actively leased ambiguous claim")
	}
	if _, err := backend.Info(ctx, jobID); err == nil {
		t.Fatal("DeleteCron created the claimed job before taking over the lease")
	}

	if err := <-result; err != nil {
		t.Fatalf("DeleteCron() error = %v", err)
	}
	if backend.cronClaims.Exists(ctx, key) {
		t.Fatal("DeleteCron retained the reconciled claim")
	}
	if _, err := backend.Info(ctx, jobID); err == nil {
		t.Fatal("DeleteCron rebuilt a job from a stale registration revision")
	}
	if _, err := backend.cronStore.Get(ctx, name); err == nil {
		t.Fatal("DeleteCron retained the disabled registration")
	}
}

func TestClaimCronOccurrence_StaleRegistrationCannotOutraceDelete(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-stale-registration-" + core.NewUUIDv7()
	name := "cron-stale-registration-" + core.NewUUIDv7()
	occurrence := registerDueCron(t, backend, name, queue)
	stale, err := backend.cronStore.GetEntry(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.GetEntry() error = %v", err)
	}

	if _, err := backend.DeleteCron(ctx, name); err != nil {
		t.Fatalf("DeleteCron() error = %v", err)
	}
	if err := backend.claimCronOccurrence(ctx, stale.Cron, occurrence, stale.Revision); err != nil {
		t.Fatalf("claimCronOccurrence(stale registration) error = %v", err)
	}
	if backend.cronClaims.Exists(ctx, cronOccurrenceKey(name, occurrence)) {
		t.Fatal("stale scheduler retained a claim after cron deletion")
	}
	if jobs, err := backend.Fetch(ctx, []string{queue}, 1, "stale-registration-worker", 5000); err != nil || len(jobs) != 0 {
		t.Fatalf("stale scheduler enqueued after cron deletion: jobs=%d err=%v", len(jobs), err)
	}
}

func TestReconcileCronClaim_RegistrationRevisionFencesRebuild(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	queue := "it-cron-revision-fence-" + core.NewUUIDv7()
	name := "cron-revision-fence-" + core.NewUUIDv7()
	occurrence := registerDueCron(t, backend, name, queue)
	stale, err := backend.cronStore.GetEntry(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.GetEntry(stale) error = %v", err)
	}

	current := *stale.Cron
	current.JobTemplate = &core.CronJobTemplate{
		Type: "cron.revised",
		Args: json.RawMessage(`["current"]`),
		Options: &core.EnqueueOptions{
			Queue: queue,
		},
	}
	if err := backend.cronStore.Register(ctx, &current); err != nil {
		t.Fatalf("cronStore.Register(revised) error = %v", err)
	}

	jobID := core.NewUUIDv7()
	claim := cronOccurrenceClaim{
		CronName:     name,
		OccurrenceAt: core.FormatTime(occurrence),
		JobID:        jobID,
		Status:       "pending",
		Owner:        "stale-replica",
		LeaseUntil:   core.FormatTime(time.Now().Add(-time.Second)),
		ClaimedAt:    core.FormatTime(time.Now().Add(-2 * time.Second)),
		CronRevision: stale.Revision,
	}
	data, err := json.Marshal(claim)
	if err != nil {
		t.Fatalf("json.Marshal(stale claim) error = %v", err)
	}
	key := cronOccurrenceKey(name, occurrence)
	if _, err := backend.cronClaims.Create(ctx, key, data); err != nil {
		t.Fatalf("cronClaims.Create(stale claim) error = %v", err)
	}

	if err := backend.reconcileCronClaim(ctx, key); err != nil {
		t.Fatalf("reconcileCronClaim(stale revision) error = %v", err)
	}
	if backend.cronClaims.Exists(ctx, key) {
		t.Fatal("stale revision claim remained after reconciliation")
	}
	if _, err := backend.Info(ctx, jobID); err == nil {
		t.Fatal("stale revision claim rebuilt its job")
	}
	unchanged, err := backend.cronStore.GetEntry(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.GetEntry(current) error = %v", err)
	}
	if unchanged.Cron.NextRunAt != current.NextRunAt {
		t.Fatalf("stale revision advanced current cursor to %q", unchanged.Cron.NextRunAt)
	}

	if err := backend.FireCronJobs(ctx); err != nil {
		t.Fatalf("FireCronJobs(current revision) error = %v", err)
	}
	fetched := fetchJobEventually(t, backend, queue, "cron-revision-worker")
	if fetched.Type != "cron.revised" || string(fetched.Args) != `["current"]` {
		t.Fatalf("fetched revised job = type %q args %s", fetched.Type, fetched.Args)
	}
	if _, err := backend.Ack(ctx, fetched.ID, nil); err != nil {
		t.Fatalf("Ack() error = %v", err)
	}
}

func TestCronClaimScan_IsProportionalToInflightClaims(t *testing.T) {
	backend := newIntegrationBackend(t)
	ctx := context.Background()
	baseline, err := backend.cronClaimKeys(ctx)
	if err != nil {
		t.Fatalf("cronClaimKeys() baseline error = %v", err)
	}
	unrelatedPrefix := "cron-scan-unrelated." + core.NewUUIDv7() + "."
	var cleanup []struct {
		key      string
		revision uint64
	}
	t.Cleanup(func() {
		for _, entry := range cleanup {
			_ = backend.cronClaims.DeleteRevision(context.Background(), entry.key, entry.revision)
		}
	})

	for i := 0; i < 128; i++ {
		key := unrelatedPrefix + core.NewUUIDv7()
		revision, err := backend.cronClaims.Create(ctx, key, []byte("unrelated"))
		if err != nil {
			t.Fatalf("cronClaims.Create(unrelated %d) error = %v", i, err)
		}
		cleanup = append(cleanup, struct {
			key      string
			revision uint64
		}{key: key, revision: revision})
	}
	for i := 0; i < 3; i++ {
		occurrence := time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		claim := cronOccurrenceClaim{
			CronName:     "cron-scan-" + core.NewUUIDv7(),
			OccurrenceAt: core.FormatTime(occurrence),
			Status:       "pending",
			Owner:        "other-replica",
			LeaseUntil:   core.FormatTime(time.Now().Add(time.Hour)),
			ClaimedAt:    core.NowFormatted(),
		}
		data, err := json.Marshal(claim)
		if err != nil {
			t.Fatalf("json.Marshal(claim %d) error = %v", i, err)
		}
		key := cronOccurrenceKey(claim.CronName, occurrence)
		revision, err := backend.cronClaims.Create(ctx, key, data)
		if err != nil {
			t.Fatalf("cronClaims.Create(claim %d) error = %v", i, err)
		}
		cleanup = append(cleanup, struct {
			key      string
			revision uint64
		}{key: key, revision: revision})
	}

	keys, err := backend.cronClaimKeys(ctx)
	if err != nil {
		t.Fatalf("cronClaimKeys() error = %v", err)
	}
	if got := len(keys) - len(baseline); got != 3 {
		t.Fatalf("cron claim scan grew by %d keys with 3 in-flight and 128 unrelated keys", got)
	}
	if err := backend.reconcileCronClaims(ctx); err != nil {
		t.Fatalf("reconcileCronClaims() error = %v", err)
	}
}

func BenchmarkReconcileCronClaimsInflight(b *testing.B) {
	natsURL := os.Getenv("NATS_URL")
	if natsURL == "" {
		natsURL = "nats://localhost:4222"
	}
	backend, err := New(natsURL)
	if err != nil {
		b.Skipf("NATS unavailable at %s: %v", natsURL, err)
	}
	defer func() {
		_ = backend.Close()
	}()

	ctx := context.Background()
	baseline, err := backend.cronClaimKeys(ctx)
	if err != nil {
		b.Fatalf("cronClaimKeys() baseline error = %v", err)
	}
	var cleanup []struct {
		key      string
		revision uint64
	}
	defer func() {
		for _, entry := range cleanup {
			_ = backend.cronClaims.DeleteRevision(context.Background(), entry.key, entry.revision)
		}
	}()
	for i := 0; i < 1000; i++ {
		key := "cron-benchmark-unrelated." + core.NewUUIDv7()
		revision, createErr := backend.cronClaims.Create(ctx, key, []byte("unrelated"))
		if createErr != nil {
			b.Fatalf("cronClaims.Create(unrelated %d) error = %v", i, createErr)
		}
		cleanup = append(cleanup, struct {
			key      string
			revision uint64
		}{key: key, revision: revision})
	}
	const inflight = 8
	for i := 0; i < inflight; i++ {
		occurrence := time.Now().UTC().Add(time.Duration(i) * time.Millisecond)
		claim := cronOccurrenceClaim{
			CronName:     "cron-benchmark-" + core.NewUUIDv7(),
			OccurrenceAt: core.FormatTime(occurrence),
			Status:       "pending",
			Owner:        "other-replica",
			LeaseUntil:   core.FormatTime(time.Now().Add(time.Hour)),
			ClaimedAt:    core.NowFormatted(),
		}
		data, marshalErr := json.Marshal(claim)
		if marshalErr != nil {
			b.Fatalf("json.Marshal(claim %d) error = %v", i, marshalErr)
		}
		key := cronOccurrenceKey(claim.CronName, occurrence)
		revision, createErr := backend.cronClaims.Create(ctx, key, data)
		if createErr != nil {
			b.Fatalf("cronClaims.Create(claim %d) error = %v", i, createErr)
		}
		cleanup = append(cleanup, struct {
			key      string
			revision uint64
		}{key: key, revision: revision})
	}

	keys, err := backend.cronClaimKeys(ctx)
	if err != nil {
		b.Fatalf("cronClaimKeys() error = %v", err)
	}
	if got := len(keys) - len(baseline); got != inflight {
		b.Fatalf("cronClaimKeys() grew by %d, want %d", got, inflight)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := backend.reconcileCronClaims(ctx); err != nil {
			b.Fatalf("reconcileCronClaims() error = %v", err)
		}
	}
	b.ReportMetric(inflight, "claims/op")
}

func registerDueCron(t *testing.T, backend *NATSBackend, name, queue string) time.Time {
	t.Helper()
	ctx := context.Background()
	_, err := backend.RegisterCron(ctx, &core.CronJob{
		Name:       name,
		Expression: "* * * * *",
		Timezone:   "UTC",
		JobTemplate: &core.CronJobTemplate{
			Type: "cron.run",
			Args: json.RawMessage(`[]`),
			Options: &core.EnqueueOptions{
				Queue: queue,
			},
		},
	})
	if err != nil {
		t.Fatalf("RegisterCron() error = %v", err)
	}
	t.Cleanup(func() {
		_, _ = backend.DeleteCron(context.Background(), name)
	})
	entry, err := backend.cronStore.Get(ctx, name)
	if err != nil {
		t.Fatalf("cronStore.Get() error = %v", err)
	}
	due := time.Now().UTC().Truncate(time.Minute)
	entry.NextRunAt = core.FormatTime(due)
	if err := backend.cronStore.Register(ctx, entry); err != nil {
		t.Fatalf("cronStore.Register(due) error = %v", err)
	}
	return due
}

func cronClaimsFor(t *testing.T, backend *NATSBackend, name string) []cronOccurrenceClaim {
	t.Helper()
	keys, err := backend.cronClaims.Keys(context.Background())
	if err != nil {
		t.Fatalf("cronClaims.Keys() error = %v", err)
	}
	var claims []cronOccurrenceClaim
	for _, key := range keys {
		if !strings.HasPrefix(key, cronClaimPrefix) {
			continue
		}
		claim, _, err := backend.getCronClaim(context.Background(), key)
		if err == nil && claim.CronName == name {
			claims = append(claims, claim)
		}
	}
	return claims
}

func waitForKVHistoryGone(
	t *testing.T,
	store jetstream.KeyValue,
	key string,
	timeout time.Duration,
) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		_, err := store.History(context.Background(), key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("History(%q) error = %v", key, err)
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("History(%q) still retained after %v", key, timeout)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
