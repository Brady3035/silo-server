package historyimport

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

// fakeSeriesDrops is a SeriesDropStore over maps. Drops it imports are
// recorded in imported and become active drops.
type fakeSeriesDrops struct {
	seriesOf   map[string]string // episode item ID -> series ID
	dropped    map[string]catalog.DroppedSeries
	activity   map[string]time.Time
	resolveErr error

	resolveCalls int
	imported     []importedDrop
}

type importedDrop struct {
	seriesID  string
	droppedAt time.Time
	observed  *time.Time
}

func (f *fakeSeriesDrops) ResolveDropSeries(_ context.Context, itemID string) (string, bool, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return "", false, f.resolveErr
	}
	seriesID, ok := f.seriesOf[itemID]
	return seriesID, ok, nil
}

func (f *fakeSeriesDrops) ListDropped(_ context.Context, _ int, _ string, seriesIDs []string) ([]catalog.DroppedSeries, error) {
	var out []catalog.DroppedSeries
	for _, id := range seriesIDs {
		if drop, ok := f.dropped[id]; ok {
			out = append(out, drop)
		}
	}
	return out, nil
}

func (f *fakeSeriesDrops) LatestActivity(_ context.Context, _ int, _ string, seriesIDs []string) (map[string]time.Time, error) {
	out := make(map[string]time.Time)
	for _, id := range seriesIDs {
		if at, ok := f.activity[id]; ok {
			out[id] = at
		}
	}
	return out, nil
}

func (f *fakeSeriesDrops) ImportDrop(_ context.Context, _ int, _ string, seriesID string, droppedAt time.Time, observed *time.Time) (bool, error) {
	f.imported = append(f.imported, importedDrop{seriesID: seriesID, droppedAt: droppedAt, observed: observed})
	return true, nil
}

func (f *fakeSeriesDrops) importedIDs() []string {
	ids := make([]string, 0, len(f.imported))
	for _, drop := range f.imported {
		ids = append(ids, drop.seriesID)
	}
	return ids
}

// fakeNextUp surfaces one episode for each series in surfaced.
type fakeNextUp struct {
	surfaced map[string]bool
	queries  []catalog.NextUpQuery
}

func (f *fakeNextUp) ListNextUp(_ context.Context, q catalog.NextUpQuery) ([]catalog.NextUpResult, error) {
	f.queries = append(f.queries, q)
	if f.surfaced[q.SeriesID] {
		return []catalog.NextUpResult{{ContentID: q.SeriesID + "-next", SeriesID: q.SeriesID}}, nil
	}
	return nil, nil
}

func reconcileService(drops *fakeSeriesDrops, nextUp *fakeNextUp, matcherRepo *matcherRepoStub) *Service {
	if matcherRepo == nil {
		matcherRepo = &matcherRepoStub{}
	}
	return &Service{matcher: NewMatcher(matcherRepo), seriesDrops: drops, nextUp: nextUp}
}

func TestReconcileContinueWatchingDropsShowsTheRowLeavesOut(t *testing.T) {
	t.Parallel()

	older := time.Date(2026, 9, 1, 20, 0, 0, 0, time.UTC)
	newest := time.Date(2026, 10, 1, 21, 30, 15, 123_456_789, time.UTC)
	drops := &fakeSeriesDrops{seriesOf: map[string]string{
		"bodkin-1": "s-bodkin", "bodkin-2": "s-bodkin",
		"lasso-3":   "s-lasso",
		"beef-hd-2": "s-beef",
		"done-9":    "s-done",
	}}
	nextUp := &fakeNextUp{surfaced: map[string]bool{"s-bodkin": true, "s-lasso": true, "s-beef": true}}
	// BEEF is listed only through a copy the run imported nothing from, so
	// it is recognized by its TMDB ID.
	matcherRepo := &matcherRepoStub{mediaByExternal: map[string][]mediaLookupRow{
		"series:tmdb_id:154385": {{ContentID: "s-beef", Title: "BEEF"}},
	}}
	service := reconcileService(drops, nextUp, matcherRepo)

	row := ContinueWatchingRow{
		SourceSeriesIDs: map[string]bool{"lasso": true, "beef": true},
		Series:          []Record{{ExternalID: "beef", Kind: KindSeries, TMDBID: "154385", PreferTMDB: true}},
	}
	episodes := []importedEpisode{
		{itemID: "bodkin-1", sourceSeriesID: "bodkin", at: older},
		{itemID: "bodkin-2", sourceSeriesID: "bodkin", at: newest},
		{itemID: "lasso-3", sourceSeriesID: "lasso", at: older},
		{itemID: "beef-hd-2", sourceSeriesID: "beef-hd", at: older},
		// Finished: Silo surfaces nothing for it, so it stays undropped and
		// its next season will show.
		{itemID: "done-9", sourceSeriesID: "done", at: older},
	}

	dropped, err := service.reconcileContinueWatching(context.Background(), 7, "profile-1", row, episodes)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if dropped != 1 || !slices.Equal(drops.importedIDs(), []string{"s-bodkin"}) {
		t.Fatalf("dropped %d %v, want only s-bodkin", dropped, drops.importedIDs())
	}
	// The drop is dated at the show's newest imported play, as Postgres
	// stores it, so later playback ends it.
	if got := drops.imported[0]; !got.droppedAt.Equal(newest.Truncate(time.Microsecond)) || got.observed != nil {
		t.Fatalf("drop = %+v, want dated %v with no observed row", got, newest.Truncate(time.Microsecond))
	}
	// One episode per source series is resolved.
	if drops.resolveCalls != 4 {
		t.Fatalf("resolve calls = %d, want 4 (one per show)", drops.resolveCalls)
	}
	for _, q := range nextUp.queries {
		if q.UserID != 7 || q.ProfileID != "profile-1" || q.Limit != 1 || !q.EnableResumable {
			t.Fatalf("next-up query = %+v, want a series-scoped lookup including resumable episodes", q)
		}
		if q.SeriesID == "s-lasso" || q.SeriesID == "s-beef" {
			t.Fatalf("listed show %s was looked up, want listed shows skipped", q.SeriesID)
		}
	}
}

func TestReconcileContinueWatchingKeepsTheProfilesOwnChoices(t *testing.T) {
	t.Parallel()

	imported := time.Date(2026, 10, 1, 21, 0, 0, 0, time.UTC)
	before := imported.Add(-time.Hour)
	after := imported.Add(time.Hour)
	drops := &fakeSeriesDrops{
		seriesOf: map[string]string{"a": "s-watched-here", "b": "s-dropped", "c": "s-redropped", "d": "s-ended", "e": "s-undone"},
		dropped: map[string]catalog.DroppedSeries{
			"s-dropped":   {SeriesID: "s-dropped", DroppedAt: before, Active: true},
			"s-redropped": {SeriesID: "s-redropped", DroppedAt: after, Active: true},
			// Ended before the imported play: the source's newer hide wins.
			"s-ended": {SeriesID: "s-ended", DroppedAt: before, Active: false},
			// Dropped and watched again after the imported play: the profile
			// already undid a newer drop, which stands.
			"s-undone": {SeriesID: "s-undone", DroppedAt: after, Active: false},
		},
		// Watched in Silo after the source's last play.
		activity: map[string]time.Time{"s-watched-here": after, "s-ended": imported},
	}
	nextUp := &fakeNextUp{surfaced: map[string]bool{
		"s-watched-here": true, "s-dropped": true, "s-redropped": true, "s-ended": true, "s-undone": true,
	}}
	service := reconcileService(drops, nextUp, nil)

	var episodes []importedEpisode
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		episodes = append(episodes, importedEpisode{itemID: id, sourceSeriesID: "src-" + id, at: imported})
	}
	dropped, err := service.reconcileContinueWatching(context.Background(), 7, "profile-1", ContinueWatchingRow{}, episodes)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if dropped != 1 || !slices.Equal(drops.importedIDs(), []string{"s-ended"}) {
		t.Fatalf("dropped %d %v, want only s-ended re-dropped", dropped, drops.importedIDs())
	}
	if got := drops.imported[0]; got.observed == nil || !got.observed.Equal(before) || !got.droppedAt.Equal(imported) {
		t.Fatalf("re-drop = %+v, want dated %v replacing the ended drop of %v", got, imported, before)
	}
}

func TestReconcileContinueWatchingDatesUndatedImportsAtTheEpoch(t *testing.T) {
	t.Parallel()

	drops := &fakeSeriesDrops{seriesOf: map[string]string{"ep": "s-1"}}
	service := reconcileService(drops, &fakeNextUp{surfaced: map[string]bool{"s-1": true}}, nil)

	episodes := []importedEpisode{newImportedEpisode("ep", Record{Kind: KindEpisode, SourceSeriesID: "src"})}
	if _, err := service.reconcileContinueWatching(context.Background(), 7, "p", ContinueWatchingRow{}, episodes); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(drops.imported) != 1 || !drops.imported[0].droppedAt.Equal(undatedImportTime) {
		t.Fatalf("drops = %+v, want one dated at the epoch so any real playback ends it", drops.imported)
	}
}

func TestReconcileContinueWatchingDoesNothingWithoutItsStores(t *testing.T) {
	t.Parallel()

	episodes := []importedEpisode{{itemID: "ep", sourceSeriesID: "src", at: time.Now()}}
	dropped, err := (&Service{}).reconcileContinueWatching(context.Background(), 7, "p", ContinueWatchingRow{}, episodes)
	if err != nil || dropped != 0 {
		t.Fatalf("reconcile without stores = %d, %v; want 0, nil", dropped, err)
	}
}

func TestReconcileContinueWatchingStopsOnStoreErrors(t *testing.T) {
	t.Parallel()

	failure := errors.New("database unavailable")
	drops := &fakeSeriesDrops{resolveErr: failure}
	service := reconcileService(drops, &fakeNextUp{}, nil)
	episodes := []importedEpisode{{itemID: "ep", sourceSeriesID: "src", at: time.Now()}}
	if _, err := service.reconcileContinueWatching(context.Background(), 7, "p", ContinueWatchingRow{}, episodes); !errors.Is(err, failure) {
		t.Fatalf("err = %v, want %v", err, failure)
	}
	if len(drops.imported) != 0 {
		t.Fatalf("drops after an error = %v, want none", drops.importedIDs())
	}
}

// rowProvider is a Provider reporting a fixed row.
type rowProvider struct {
	row ContinueWatchingRow
	ok  bool
}

func (rowProvider) Fetch(context.Context) ([]Record, []string, error) { return nil, nil, nil }

func (p rowProvider) ContinueWatchingRow() (ContinueWatchingRow, bool) { return p.row, p.ok }

// Queued runs wrap their provider for private network access; the run reads
// the row through that wrapper, so it must pass the row on.
func TestPrivateNetworkProviderPassesTheRowThrough(t *testing.T) {
	t.Parallel()

	want := ContinueWatchingRow{SourceSeriesIDs: map[string]bool{"lasso": true}}
	var wrapped Provider = privateNetworkProvider{rowProvider{row: want, ok: true}}
	reporter, ok := wrapped.(ContinueWatchingRowReporter)
	if !ok {
		t.Fatal("wrapped provider does not report a row")
	}
	if row, ok := reporter.ContinueWatchingRow(); !ok || !row.SourceSeriesIDs["lasso"] {
		t.Fatalf("wrapped row = %+v, %v; want the inner provider's row", row, ok)
	}

	// The wrapped Emby provider, as queued runs build it.
	if _, ok := Provider(privateNetworkProvider{NewEmbyProvider(nil, embyLocalAuth{})}).(ContinueWatchingRowReporter); !ok {
		t.Fatal("wrapped Emby provider does not report a row")
	}

	// A provider without a row reports none through the wrapper.
	if _, ok := (privateNetworkProvider{staticWatchlistProvider{}}).ContinueWatchingRow(); ok {
		t.Fatal("wrapped provider without a row reported one")
	}
}
