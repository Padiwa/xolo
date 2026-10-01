package cache

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/port"
)

// countingUsageStore records how often the budget totals were actually read
// from the backend, which is the whole point of the cache in front of it.
type countingUsageStore struct {
	port.UsageStore

	// The concurrency tests below call both methods at once, so the fake has to
	// be safe itself; without this the race detector reports the fake, not the
	// decorator under test.
	mu    sync.Mutex
	total int64
	reads int

	recorded []model.UsageRecord
}

func (s *countingUsageStore) SumQuotaCostSince(_ context.Context, _ model.QuotaScope, _ string, _ model.OrgID, _ time.Time) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.reads++
	return s.total, nil
}

func (s *countingUsageStore) RecordUsage(_ context.Context, record model.UsageRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.recorded = append(s.recorded, record)
	s.total += record.Cost()
	return nil
}

func (s *countingUsageStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.reads
}

func newPAYGRecord(userID model.UserID, orgID model.OrgID, cost int64) *model.BaseUsageRecord {
	return model.NewUsageRecord(userID, "", orgID, "provider", "llm-model",
		"fast", "", 10, 0, 10, cost, "USD", model.CostSourceComputed, "")
}

// datedRecord pins a record's CreatedAt() without exposing a mutation setter on
// the domain type. quotaSumCacheKeysFor and quotaUsageRows both derive their
// day/month/year windows from record.CreatedAt(), and fromUsageRecord does not
// persist it (GORM stamps created_at at insert time), so a public SetCreatedAt
// would let production callers split the cache windows from the persisted
// counter day. The wrapper is enough because RecordUsage and
// quotaSumCacheKeysFor take the model.UsageRecord interface, and every other
// method forwards to the wrapped value.
type datedRecord struct {
	model.UsageRecord
	at time.Time
}

func (r datedRecord) CreatedAt() time.Time { return r.at }

func TestUsageStore_CachesBudgetTotals(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{total: 5_000}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	// Pin both windows to fixed mid-month values. On 1 January StartOfDay and
	// StartOfYear return the same instant, the two SumQuotaCostSince calls
	// would hit the same cache entry, and the readCount assertion below would
	// fire on the very date the dedup regression test targets.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)
	since := model.StartOfDay(midMonth)
	for i := 0; i < 3; i++ {
		total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
		if err != nil {
			t.Fatalf("SumQuotaCostSince: %v", err)
		}
		if total != 5_000 {
			t.Fatalf("total = %d, want 5000", total)
		}
	}
	if backend.readCount() != 1 {
		t.Errorf("backend read %d times, want 1: the total must be reused within the TTL", backend.readCount())
	}

	// A different window is a different entry, not a cache hit.
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", model.StartOfYear(midMonth)); err != nil {
		t.Fatalf("SumQuotaCostSince (year): %v", err)
	}
	if backend.readCount() != 2 {
		t.Errorf("backend read %d times, want 2: each window has its own total", backend.readCount())
	}
}

// TestUsageStore_RecordUsageUpdatesCachedTotals is what keeps a budget from
// being overshot for the length of a TTL: the spending of a request is applied
// to the totals the next one will read.
func TestUsageStore_RecordUsageUpdatesCachedTotals(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{total: 1_000}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	// Pin the record's timestamp to the middle of a month so the day, month
	// and year windows are distinct. On the 1st of a month StartOfDay equals
	// StartOfMonth (and on 1 January, StartOfYear too), which is the case the
	// dedup regression tests cover below; here we want a baseline.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)
	since := model.StartOfDay(midMonth)
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", since); err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since); err != nil {
		t.Fatalf("SumQuotaCostSince (org): %v", err)
	}

	record := newPAYGRecord("user-1", "org-1", 250)
	if err := store.RecordUsage(ctx, datedRecord{record, midMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	readsBefore := backend.readCount()
	userTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}
	if userTotal != 1_250 {
		t.Errorf("user total = %d, want 1250: the record must be applied to the cached total", userTotal)
	}
	orgTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (org): %v", err)
	}
	if orgTotal != 1_250 {
		t.Errorf("org total = %d, want 1250", orgTotal)
	}
	if backend.readCount() != readsBefore {
		t.Errorf("backend read again (%d → %d): the increment must not invalidate the entry", readsBefore, backend.readCount())
	}
}

// TestUsageStore_IgnoresNonBudgetRecords mirrors what the counters themselves
// do: subscription-covered and zero-cost usage consumes no monetary budget, so
// it must not move a cached total either.
func TestUsageStore_IgnoresNonBudgetRecords(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{total: 1_000}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	since := model.StartOfDay(time.Now())
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since); err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}

	planned := newPAYGRecord("user-1", "org-1", 500)
	planned.SetPlanCovered(true)
	if err := store.RecordUsage(ctx, planned); err != nil {
		t.Fatalf("RecordUsage (plan): %v", err)
	}
	if err := store.RecordUsage(ctx, newPAYGRecord("user-1", "org-1", 0)); err != nil {
		t.Fatalf("RecordUsage (free): %v", err)
	}

	total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}
	if total != 1_000 {
		t.Errorf("total = %d, want 1000: non-budget usage must not move it", total)
	}
}

// TestUsageStore_KeysFollowThePrincipalOnTheRecord pins which totals a record
// feeds, for the two shapes a record actually takes.
//
// An application authenticates through a shadow user, so in production its
// records carry that user's id. The two are distinct budgets: the application
// counter is what the enforcer checks once a budget is set on the application
// (issue #64), and the shadow user's counter is left alone so a human call on
// the same token does not double-count. A record with no principal at all
// feeds the organization only.
func TestUsageStore_KeysFollowThePrincipalOnTheRecord(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	// Pin the record's timestamp to mid-month so the day, month and year
	// windows stay distinct. On the 1st of a month some of those windows
	// collapse and the assertion below would catch a healthy deduplication as
	// a regression.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)

	// An application call as the auth extractor produces it: the shadow user's
	// id in UserID, the application's own id alongside.
	shadow := model.NewUsageRecord("usr-shadow-app-1", "app-1", "org-1", "provider", "llm-model",
		"fast", "", 10, 0, 10, 700, "USD", model.CostSourceComputed, "")

	day := model.StartOfDay(midMonth)
	keys := quotaSumCacheKeysFor(datedRecord{shadow, midMonth})
	if len(keys) != 6 {
		t.Errorf("keys = %v, want three organization windows and three application windows, no shadow-user windows", keys)
	}
	wantAppKey := quotaSumCacheKey(model.QuotaScopeApplication, "app-1", "org-1", day)
	if !slices.Contains(keys, wantAppKey) {
		t.Errorf("keys = %v, want one under the application %q: the budget the enforcer checks", keys, wantAppKey)
	}
	// The shadow user has no budget set, so attributing its spending to it
	// would silently bypass the application-level cap.
	if slices.Contains(keys, quotaSumCacheKey(model.QuotaScopeUser, "usr-shadow-app-1", "org-1", day)) {
		t.Errorf("keys = %v, want no total under the shadow user", keys)
	}

	if err := store.RecordUsage(ctx, datedRecord{shadow, midMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	// A record with no principal at all — possible if a caller hands a UsageRecord
	// directly without an auth context — feeds the organization windows only.
	orphan := model.NewUsageRecord("", "", "org-1", "provider", "llm-model",
		"fast", "", 10, 0, 10, 700, "USD", model.CostSourceComputed, "")
	if keys := quotaSumCacheKeysFor(datedRecord{orphan, midMonth}); len(keys) != 3 {
		t.Errorf("keys = %v, want the three organization windows only", keys)
	}
}

// blockingUsageStore holds SumQuotaCostSince open until the test releases it,
// so the window between reading a total and storing it can be driven by hand.
type blockingUsageStore struct {
	port.UsageStore

	total int64

	entered  chan struct{}
	release  chan struct{}
	blockOne bool
}

func (s *blockingUsageStore) SumQuotaCostSince(_ context.Context, _ model.QuotaScope, _ string, _ model.OrgID, _ time.Time) (int64, error) {
	if s.blockOne {
		s.blockOne = false
		// The answer is fixed before the caller is released, the way a database
		// read that started earlier does not see a later commit.
		answer := s.total
		s.entered <- struct{}{}
		<-s.release
		return answer, nil
	}
	return s.total, nil
}

func (s *blockingUsageStore) RecordUsage(_ context.Context, record model.UsageRecord) error {
	s.total += record.Cost()
	return nil
}

// TestUsageStore_DropsTotalReadBeforeAConcurrentRecord pins the guard the whole
// mechanism exists for. A total read before a record was written must not be
// stored after that record's increment ran: the increment finds no entry to
// apply itself to, so storing the total would hide the record's cost until the
// TTL elapsed, and a budget could be overshot.
func TestUsageStore_DropsTotalReadBeforeAConcurrentRecord(t *testing.T) {
	ctx := context.Background()
	backend := &blockingUsageStore{
		total:    1_000,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		blockOne: true,
	}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)
	// Pin the record's timestamp to mid-month so day, month and year stay
	// distinct; on the 1st of a month those windows collapse and the
	// dedup regression tests would mask this one.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)
	since := model.StartOfDay(midMonth)

	read := make(chan int64)
	go func() {
		total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
		if err != nil {
			t.Errorf("SumQuotaCostSince: %v", err)
		}
		read <- total
	}()

	// The read has fetched nothing yet; record while it is held open.
	<-backend.entered
	record := newPAYGRecord("user-1", "org-1", 250)
	if err := store.RecordUsage(ctx, datedRecord{record, midMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	backend.release <- struct{}{}

	if got := <-read; got != 1_000 {
		t.Errorf("the in-flight read returned %d, want the 1000 it was answered with", got)
	}

	// The stale total must not have been cached: the next check goes back to
	// the backend and sees the record.
	total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}
	if total != 1_250 {
		t.Errorf("total = %d, want 1250: the total read before the record was cached anyway", total)
	}
}

// TestUsageStore_DropsApplicationTotalReadBeforeAConcurrentRecord is the
// application-scope mirror of the test above. QuotaScopeApplication uses the
// same locking protocol as QuotaScopeOrg, but a regression that mishandles
// the new scope would slip past the org-only test, and a bug here would let
// an application request slip past the application budget on the hot path.
func TestUsageStore_DropsApplicationTotalReadBeforeAConcurrentRecord(t *testing.T) {
	ctx := context.Background()
	backend := &blockingUsageStore{
		total:    1_000,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		blockOne: true,
	}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)
	// Pin the record's timestamp to mid-month so day, month and year stay
	// distinct.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)
	since := model.StartOfDay(midMonth)

	read := make(chan int64)
	go func() {
		total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeApplication, "app-1", "org-1", since)
		if err != nil {
			t.Errorf("SumQuotaCostSince: %v", err)
		}
		read <- total
	}()

	<-backend.entered
	// Record an application call (the only kind that should affect the
	// application counter): application scope id, no user id.
	record := model.NewUsageRecord("", "app-1", "org-1", "provider", "llm-model",
		"fast", "", 10, 0, 10, 250, "USD", model.CostSourceComputed, "")
	if err := store.RecordUsage(ctx, datedRecord{record, midMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}
	backend.release <- struct{}{}

	if got := <-read; got != 1_000 {
		t.Errorf("the in-flight application read returned %d, want the 1000 it was answered with", got)
	}

	// The stale application total must not have been cached.
	total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeApplication, "app-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}
	if total != 1_250 {
		t.Errorf("total = %d, want 1250: the application total read before the record was cached anyway", total)
	}
}

// TestUsageStore_KeepsTotalReadWithoutConcurrentRecord is the other half: a
// read nothing interfered with must still be cached, or the cache would never
// serve anything under load.
func TestUsageStore_KeepsTotalReadWithoutConcurrentRecord(t *testing.T) {
	ctx := context.Background()
	backend := &blockingUsageStore{
		total:    1_000,
		entered:  make(chan struct{}),
		release:  make(chan struct{}),
		blockOne: true,
	}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)
	// Pin the read's window to the middle of a month so day, month and year
	// stays are distinct: the regression tests below cover the 1st-of-month
	// and 1 January cases where some of those windows collapse.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)
	since := model.StartOfDay(midMonth)

	read := make(chan struct{})
	go func() {
		if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since); err != nil {
			t.Errorf("SumQuotaCostSince: %v", err)
		}
		close(read)
	}()

	<-backend.entered
	backend.release <- struct{}{}
	<-read

	// A record landing after the read was stored is applied in place, so the
	// total stays exact without going back to the backend. Pin its timestamp
	// to mid-month so the three budget windows stay distinct.
	record := newPAYGRecord("user-1", "org-1", 250)
	if err := store.RecordUsage(ctx, datedRecord{record, midMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	backend.total = 999_999 // any backend read from here on would be visible
	total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince: %v", err)
	}
	if total != 1_250 {
		t.Errorf("total = %d, want 1250 served from the cache", total)
	}
}

// TestUsageStore_ConcurrentReadsAndRecords runs the two paths against each
// other under -race, and checks that whatever the interleaving, the cached
// total never exceeds what was actually spent. Overshooting is the failure
// that refuses requests a fresh read would allow.
func TestUsageStore_ConcurrentReadsAndRecords(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)
	// Pin the record's timestamp to mid-month so day, month and year stay
	// distinct; otherwise the same window-collapse would inflate the total
	// and the sanity check below would fire on the 1st of a month.
	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)
	since := model.StartOfDay(midMonth)

	const records = 200

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		for i := 0; i < records; i++ {
			record := newPAYGRecord("user-1", "org-1", 10)
			if err := store.RecordUsage(ctx, datedRecord{record, midMonth}); err != nil {
				t.Errorf("RecordUsage: %v", err)
				return
			}
		}
	}()

	go func() {
		defer wg.Done()
		for i := 0; i < records; i++ {
			total, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
			if err != nil {
				t.Errorf("SumQuotaCostSince: %v", err)
				return
			}
			if total > records*10 {
				t.Errorf("total = %d, above the %d actually spent: a record was counted twice", total, records*10)
				return
			}
		}
	}()

	wg.Wait()
}

// TestUsageStore_RecordUsageAppliesOnceWhenDayAndMonthCollide pins the
// regression: on the 1st of a month StartOfDay equals StartOfMonth, so a
// record dated that day must apply its cost exactly once to the day total
// and exactly once to the month total, not twice to the same key. Before the
// fix RecordUsage saw the same key twice in the list and AddInt64'd it
// twice, inflating every cached total for the length of a TTL and rejecting
// requests a fresh read would allow.
func TestUsageStore_RecordUsageAppliesOnceWhenDayAndMonthCollide(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{total: 1_000}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	firstOfMonth := time.Date(2025, time.October, 1, 0, 30, 0, 0, time.UTC)

	// Prime the cache for the day window (which equals the month window) and
	// for the year window, so the increments have somewhere to land.
	day := model.StartOfDay(firstOfMonth)
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", day); err != nil {
		t.Fatalf("SumQuotaCostSince (user/day): %v", err)
	}
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", day); err != nil {
		t.Fatalf("SumQuotaCostSince (org/day): %v", err)
	}
	year := model.StartOfYear(firstOfMonth)
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", year); err != nil {
		t.Fatalf("SumQuotaCostSince (user/year): %v", err)
	}
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", year); err != nil {
		t.Fatalf("SumQuotaCostSince (org/year): %v", err)
	}

	record := newPAYGRecord("user-1", "org-1", 250)
	if err := store.RecordUsage(ctx, datedRecord{record, firstOfMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	// The day total equals the month total because the windows share their
	// start. Each must be 1000 + 250, not 1000 + 500.
	userTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", day)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (user/day): %v", err)
	}
	if userTotal != 1_250 {
		t.Errorf("user total on the 1st = %d, want 1250: a record whose day and month windows collapse must only be counted once per key", userTotal)
	}
	orgTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", day)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (org/day): %v", err)
	}
	if orgTotal != 1_250 {
		t.Errorf("org total on the 1st = %d, want 1250", orgTotal)
	}

	// The year window is distinct from the day window, so it should have
	// received its own single increment.
	yearUser, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", year)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (user/year): %v", err)
	}
	if yearUser != 1_250 {
		t.Errorf("user year total = %d, want 1250: the distinct year window must receive its own increment", yearUser)
	}
	yearOrg, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", year)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (org/year): %v", err)
	}
	if yearOrg != 1_250 {
		t.Errorf("org year total = %d, want 1250", yearOrg)
	}

	// The record should map to four distinct keys, not six: day=user/org,
	// month collapses into day, year=user/org are the four unique windows.
	keys := quotaSumCacheKeysFor(record)
	if len(keys) != 4 {
		t.Errorf("keys = %v, want 4 (day=user/org + year=user/org): the day and month windows must produce the same key", keys)
	}
}

// TestUsageStore_RecordUsageAppliesOnceWhenAllWindowsCollide is the new year's
// day variant: StartOfDay, StartOfMonth and StartOfYear all return the same
// instant, so a record dated 1 January must increment that single key once,
// not three times.
func TestUsageStore_RecordUsageAppliesOnceWhenAllWindowsCollide(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{total: 1_000}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	newYear := time.Date(2025, time.January, 1, 0, 30, 0, 0, time.UTC)

	since := model.StartOfDay(newYear)
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", since); err != nil {
		t.Fatalf("SumQuotaCostSince (user): %v", err)
	}
	if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since); err != nil {
		t.Fatalf("SumQuotaCostSince (org): %v", err)
	}

	record := newPAYGRecord("user-1", "org-1", 250)
	if err := store.RecordUsage(ctx, datedRecord{record, newYear}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	userTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (user): %v", err)
	}
	if userTotal != 1_250 {
		t.Errorf("user total on 1 January = %d, want 1250: a record dated on new year's day must be counted once per key, not three times", userTotal)
	}
	orgTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since)
	if err != nil {
		t.Fatalf("SumQuotaCostSince (org): %v", err)
	}
	if orgTotal != 1_250 {
		t.Errorf("org total on 1 January = %d, want 1250", orgTotal)
	}

	// All three windows collapse into a single user/org pair.
	keys := quotaSumCacheKeysFor(datedRecord{record, newYear})
	if len(keys) != 2 {
		t.Errorf("keys = %v, want 2 (user + org): day, month and year must produce the same key", keys)
	}
}

// TestUsageStore_RecordUsageStillUpdatesThreeWindowsMidMonth is the positive
// counterpart: when the three windows are distinct, each gets its own key and
// its own increment, so the test above did not pass for the wrong reason.
func TestUsageStore_RecordUsageStillUpdatesThreeWindowsMidMonth(t *testing.T) {
	ctx := context.Background()
	backend := &countingUsageStore{total: 1_000}
	store := NewUsageStore(backend, NewMemoryCache(64), time.Minute)

	midMonth := time.Date(2025, time.March, 15, 12, 0, 0, 0, time.UTC)

	day := model.StartOfDay(midMonth)
	month := model.StartOfMonth(midMonth)
	year := model.StartOfYear(midMonth)
	for _, since := range []time.Time{day, month, year} {
		if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", since); err != nil {
			t.Fatalf("SumQuotaCostSince (user): %v", err)
		}
		if _, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", since); err != nil {
			t.Fatalf("SumQuotaCostSince (org): %v", err)
		}
	}

	record := newPAYGRecord("user-1", "org-1", 250)
	if err := store.RecordUsage(ctx, datedRecord{record, midMonth}); err != nil {
		t.Fatalf("RecordUsage: %v", err)
	}

	for _, tc := range []struct {
		name  string
		since time.Time
		want  int64
	}{
		{"day", day, 1_250},
		{"month", month, 1_250},
		{"year", year, 1_250},
	} {
		userTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeUser, "user-1", "org-1", tc.since)
		if err != nil {
			t.Fatalf("SumQuotaCostSince (user/%s): %v", tc.name, err)
		}
		if userTotal != tc.want {
			t.Errorf("user %s total = %d, want %d", tc.name, userTotal, tc.want)
		}
		orgTotal, err := store.SumQuotaCostSince(ctx, model.QuotaScopeOrg, "org-1", "org-1", tc.since)
		if err != nil {
			t.Fatalf("SumQuotaCostSince (org/%s): %v", tc.name, err)
		}
		if orgTotal != tc.want {
			t.Errorf("org %s total = %d, want %d", tc.name, orgTotal, tc.want)
		}
	}

	// Three distinct starts, two scopes (user + org) each: six keys.
	keys := quotaSumCacheKeysFor(datedRecord{record, midMonth})
	if len(keys) != 6 {
		t.Errorf("keys = %v, want 6 (three windows × user + org): the dedup must not collapse distinct windows", keys)
	}
}
