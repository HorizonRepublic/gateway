package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/rs/zerolog"
)

// NATSKVStoreConfig is the production wiring for [NewNATSKVStore].
// Zero value is not valid: JS and HandlerBucket are mandatory.
//
// The store derives its own bucket name from HandlerBucket +
// BucketSuffix so that multiple rate-limit stores attached to
// different handler registries cannot collide in a shared NATS
// account. Replicas count is inherited from the handler registry
// bucket so the rate-limit state has the same durability class as
// the route metadata it protects — there is no useful deployment
// where routes are 3-replica and their rate-limit TAT is 1-replica.
type NATSKVStoreConfig struct {
	// JS is the JetStream handle used to create or open the
	// rate-limit bucket and to query the handler registry for its
	// replica count. MUST be a live, connected handle; constructor
	// does not retry.
	JS jetstream.JetStream

	// HandlerBucket is the name of the handler registry KV bucket
	// whose replica count the rate-limit bucket inherits. The
	// registry bucket MUST already exist — the rate-limit bucket is
	// always a downstream companion, never the first bucket in an
	// account. Empty value is rejected.
	HandlerBucket string

	// BucketSuffix is appended to HandlerBucket to form this store's
	// bucket name (e.g., "_ratelimit" → "handler_registry_ratelimit").
	// Callers MAY pass an empty suffix, but it is STRONGLY
	// discouraged — a shared bucket between routes and TAT state
	// courts subject-name collisions and makes bucket-level ops
	// (flush, status, replica changes) impossible to scope.
	BucketSuffix string

	// KeyTTL is written into the bucket's MaxAge so that a key idle
	// longer than this is automatically evicted. Chosen to be larger
	// than the GCRA tokens-to-fill time for the slowest expected
	// route (otherwise legitimate callers see the fresh-bucket path
	// after a quiet period), but small enough that a one-off spike
	// does not leak state for days. Zero value disables TTL — not
	// recommended for production.
	KeyTTL time.Duration

	// Logger is plumbed into breaker state-change logs and the
	// constructor's bucket create/reuse info lines. A nil logger
	// degrades silently; pass zerolog.Nop() explicitly if silence is
	// intended.
	Logger zerolog.Logger
}

// NewNATSKVStore builds a production-ready NATSKVStore backed by a
// JetStream KV bucket. On first call for a given HandlerBucket the
// rate-limit bucket is created with replica count inherited from the
// handler registry bucket; subsequent calls reuse the existing
// bucket (idempotent startup).
//
// Bucket configuration is fixed: History=1 (only the latest TAT
// matters), Storage=Memory (state is disposable and RAM-speed writes
// matter far more than persistence across a server restart), TTL=
// cfg.KeyTTL for automatic stale-key cleanup, and MaxValueSize sized
// to the current TAT wire length plus a small forward-compat slack.
//
// Returns an error if cfg.JS is nil, cfg.HandlerBucket is empty, the
// handler registry bucket is missing (the rate-limit bucket can
// only exist as a downstream companion), or any JetStream API call
// fails for reasons other than bucket-already-exists / bucket-not-
// found.
//
// Example:
//
//	cfg := NATSKVStoreConfig{
//		JS:            jetstream.New(natsConn),
//		HandlerBucket: "handler_registry",
//		BucketSuffix:  "_ratelimit",
//		KeyTTL:        24 * time.Hour,
//		Logger:        log,
//	}
//	store, err := NewNATSKVStore(ctx, cfg)
//	if err != nil {
//		return err
//	}
//	defer store.Close()
func NewNATSKVStore(ctx context.Context, cfg NATSKVStoreConfig) (*NATSKVStore, error) {
	if cfg.JS == nil {
		return nil, errors.New("ratelimit: NATSKVStoreConfig.JS is required")
	}

	if cfg.HandlerBucket == "" {
		return nil, errors.New("ratelimit: NATSKVStoreConfig.HandlerBucket is required")
	}

	replicas, err := inheritReplicas(ctx, cfg.JS, cfg.HandlerBucket)
	if err != nil {
		return nil, err
	}

	bucketName := cfg.HandlerBucket + cfg.BucketSuffix
	kv, created, err := openOrCreateRatelimitBucket(ctx, cfg.JS, bucketName, replicas, cfg.KeyTTL)
	if err != nil {
		return nil, err
	}

	logBucketInit(cfg.Logger, bucketName, replicas, cfg.KeyTTL, created)

	return newNATSKVStoreFromKV(&jsKVAdapter{kv: kv}, withLogger(cfg.Logger)), nil
}

// inheritReplicas reads the replica count of the handler registry
// bucket so the rate-limit bucket can match it. Uses the concrete
// KeyValueBucketStatus type to reach into StreamInfo — the
// KeyValueStatus interface does not expose replicas directly.
func inheritReplicas(ctx context.Context, js jetstream.JetStream, handlerBucket string) (int, error) {
	kv, err := js.KeyValue(ctx, handlerBucket)
	if err != nil {
		return 0, fmt.Errorf("ratelimit: open handler bucket %q: %w", handlerBucket, err)
	}

	status, err := kv.Status(ctx)
	if err != nil {
		return 0, fmt.Errorf("ratelimit: status of %q: %w", handlerBucket, err)
	}

	bucketStatus, ok := status.(*jetstream.KeyValueBucketStatus)
	if !ok {
		return 0, fmt.Errorf("ratelimit: status of %q: unexpected type %T", handlerBucket, status)
	}

	info := bucketStatus.StreamInfo()
	if info == nil {
		return 0, fmt.Errorf("ratelimit: status of %q: nil StreamInfo", handlerBucket)
	}

	return info.Config.Replicas, nil
}

// openOrCreateRatelimitBucket returns an existing rate-limit bucket
// or creates a fresh one with the provided replicas + TTL. The
// boolean return reports whether a creation happened (true) or the
// bucket was reused (false); callers use this for log framing only.
//
// Multi-replica gateway startup races on this path: two pods may see
// ErrBucketNotFound on the same KeyValue call and both issue
// CreateKeyValue, in which case the second Create fails with
// ErrBucketExists (jetstream wraps ErrStreamNameAlreadyInUse). Treat
// that loss as success by re-opening the bucket the winning pod just
// created — functionally equivalent to "we were second and the bucket
// is now available", which is exactly the idempotent-startup contract
// this function is supposed to uphold.
func openOrCreateRatelimitBucket(
	ctx context.Context,
	js jetstream.JetStream,
	bucket string,
	replicas int,
	ttl time.Duration,
) (jetstream.KeyValue, bool, error) {
	kv, err := js.KeyValue(ctx, bucket)
	if err == nil {
		return kv, false, nil
	}

	if !errors.Is(err, jetstream.ErrBucketNotFound) {
		return nil, false, fmt.Errorf("ratelimit: open bucket %q: %w", bucket, err)
	}

	kv, err = js.CreateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket:       bucket,
		History:      1,
		Storage:      jetstream.MemoryStorage,
		Replicas:     replicas,
		TTL:          ttl,
		MaxValueSize: int32(tatEncodedLength) + 64,
	})
	if err == nil {
		return kv, true, nil
	}

	if !isBucketAlreadyExistsErr(err) {
		return nil, false, fmt.Errorf("ratelimit: create bucket %q: %w", bucket, err)
	}

	// A concurrent replica won the creation race. Re-open the bucket
	// it just materialised; a subsequent ErrBucketNotFound here would
	// indicate an external deletion in the narrow window between the
	// two calls and deserves to propagate verbatim.
	kv, err = js.KeyValue(ctx, bucket)
	if err != nil {
		return nil, false, fmt.Errorf("ratelimit: reopen bucket %q after concurrent create: %w", bucket, err)
	}

	return kv, false, nil
}

// isBucketAlreadyExistsErr reports whether err signals the "bucket
// already exists" outcome from CreateKeyValue. nats.go returns
// ErrBucketExists joined with ErrStreamNameAlreadyInUse in that case;
// matching on either sentinel keeps the check stable across library
// versions that toggle the join order.
func isBucketAlreadyExistsErr(err error) bool {
	if err == nil {
		return false
	}

	return errors.Is(err, jetstream.ErrBucketExists) ||
		errors.Is(err, jetstream.ErrStreamNameAlreadyInUse)
}

// logBucketInit emits a single info line on bucket create or reuse
// so operators can confirm startup topology without tailing debug
// logs. Kept separate from the construction path so the happy-path
// function reads top-to-bottom without log noise.
func logBucketInit(logger zerolog.Logger, bucket string, replicas int, ttl time.Duration, created bool) {
	event := logger.Info().
		Str("event", "ratelimit.kv.bucket.init").
		Str("bucket", bucket).
		Int("replicas", replicas).
		Dur("max_age", ttl).
		Bool("created", created)

	if created {
		event.Msg("ratelimit KV bucket created")

		return
	}

	event.Msg("ratelimit KV bucket reused")
}

// jsKVAdapter wraps a concrete jetstream.KeyValue so it satisfies
// the package-internal kvAPI interface. Keeping the adapter thin
// lets the CAS loop stay backend-agnostic and lets tests swap in a
// fake without mocking JetStream.
type jsKVAdapter struct {
	kv jetstream.KeyValue
}

// Get returns the latest entry for key or errKVKeyNotFound if the
// key is absent. Any other error is wrapped with the operation
// context so the breaker-level log carries enough information for
// triage without consulting stack traces.
func (a *jsKVAdapter) Get(ctx context.Context, key string) (kvEntry, error) {
	entry, err := a.kv.Get(ctx, key)
	if err != nil {
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			return nil, errKVKeyNotFound
		}

		return nil, fmt.Errorf("jetstream kv get %q: %w", key, err)
	}

	return jsEntryAdapter{entry: entry}, nil
}

// Create writes a new entry or returns errCASConflict if the key
// already exists. JetStream reports this as ErrKeyExists, whose
// underlying APIError.ErrorCode is JSErrCodeStreamWrongLastSequence;
// errors.Is catches both the sentinel and the raw API error shape.
func (a *jsKVAdapter) Create(ctx context.Context, key string, value []byte) (uint64, error) {
	rev, err := a.kv.Create(ctx, key, value)
	if err != nil {
		if isJSRevisionConflict(err) {
			return 0, errCASConflict
		}

		return 0, fmt.Errorf("jetstream kv create %q: %w", key, err)
	}

	return rev, nil
}

// Update writes a new revision of an existing key or returns
// errCASConflict on revision mismatch. JetStream Update uses
// WithExpectLastSequencePerSubject under the hood, so a stale
// revision surfaces as an APIError with ErrorCode
// JSErrCodeStreamWrongLastSequence — the same code as ErrKeyExists.
// isJSRevisionConflict classifies both paths uniformly.
func (a *jsKVAdapter) Update(ctx context.Context, key string, value []byte, revision uint64) (uint64, error) {
	rev, err := a.kv.Update(ctx, key, value, revision)
	if err != nil {
		if isJSRevisionConflict(err) {
			return 0, errCASConflict
		}

		return 0, fmt.Errorf("jetstream kv update %q: %w", key, err)
	}

	return rev, nil
}

// jsEntryAdapter exposes the two fields NATSKVStore cares about —
// raw bytes and revision number — from a jetstream.KeyValueEntry.
// The entry's other metadata (Bucket, Key, Created, Delta) is
// intentionally dropped to keep the kvAPI surface minimal.
type jsEntryAdapter struct {
	entry jetstream.KeyValueEntry
}

// Value returns the stored TAT bytes.
func (e jsEntryAdapter) Value() []byte { return e.entry.Value() }

// Revision returns the monotonic sequence number used as the CAS
// precondition for subsequent Update calls.
func (e jsEntryAdapter) Revision() uint64 { return e.entry.Revision() }

// isJSRevisionConflict reports whether err is a CAS conflict
// signaled by JetStream. Matches on ErrKeyExists (Create path) and
// on any APIError carrying JSErrCodeStreamWrongLastSequence (Update
// path) — errors.Is(err, ErrKeyExists) resolves to true for both
// because APIError.Is compares ErrorCode and ErrKeyExists carries
// that very code.
//
// ErrKeyDeleted is also treated as a conflict: a key vanishing
// between Get and Update means some other writer beat us to the
// punch and a retry with a fresh Get is the correct response.
func isJSRevisionConflict(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, jetstream.ErrKeyExists) {
		return true
	}

	if errors.Is(err, jetstream.ErrKeyDeleted) {
		return true
	}

	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) && apiErr.ErrorCode == jetstream.JSErrCodeStreamWrongLastSequence {
		return true
	}

	return false
}
