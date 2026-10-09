package historyimport

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
)

// SeriesDropStore is the dropped-series store the Continue Watching pass
// writes through. notifications.DroppedSeriesTracker satisfies it, so an
// imported drop recomputes interest like a drop made in Silo.
type SeriesDropStore interface {
	ResolveDropSeries(ctx context.Context, itemID string) (string, bool, error)
	ListDropped(ctx context.Context, userID int, profileID string, seriesIDs []string) ([]catalog.DroppedSeries, error)
	LatestActivity(ctx context.Context, userID int, profileID string, seriesIDs []string) (map[string]time.Time, error)
	ImportDrop(ctx context.Context, userID int, profileID, seriesID string, droppedAt time.Time, observed *time.Time) (bool, error)
}

// NextUpLister reports what a profile's Continue Watching and Next Up would
// show for a series. catalog.NextUpRepository satisfies it.
type NextUpLister interface {
	ListNextUp(ctx context.Context, q catalog.NextUpQuery) ([]catalog.NextUpResult, error)
}

// SetContinueWatchingStores enables the pass that hides the shows a source
// hid from its own Continue Watching. Without the stores runs skip it.
func (s *Service) SetContinueWatchingStores(drops SeriesDropStore, nextUp NextUpLister) {
	s.seriesDrops = drops
	s.nextUp = nextUp
}

// importedEpisode is an episode whose watch state a run imported.
type importedEpisode struct {
	itemID         string
	sourceSeriesID string
	// at is the stamp the episode's progress was imported with.
	at time.Time
}

// newImportedEpisode records a matched episode record for the pass, stamped
// as applyImportedWatch stamps its progress.
func newImportedEpisode(itemID string, record Record) importedEpisode {
	at := record.UpdatedAt
	if at.IsZero() {
		at = undatedImportTime
	}
	return importedEpisode{itemID: itemID, sourceSeriesID: record.SourceSeriesID, at: at}
}

// reconcileContinueWatching drops each show the run put in the profile's
// Continue Watching or Next Up that the source's own row leaves out. Emby
// hides per show, both a show paused mid-episode and one between episodes,
// and keeps it hidden until one of its episodes is played again; a Silo drop
// behaves the same way. It returns how many shows it dropped.
//
// Each drop is dated at the show's newest imported play, not at the time of
// the run, so it never outlasts playback newer than that, in Silo or at the
// source on a later run: a drop ends at the first progress newer than it.
// That also keeps the pass idempotent, and it never touches a show the user
// has watched in Silo since the source's last play.
//
// A show counts as listed when any of its copies at the source is in the
// row: by the source series of the episodes the run imported, or by the
// listed series' provider IDs.
func (s *Service) reconcileContinueWatching(ctx context.Context, userID int, profileID string, row ContinueWatchingRow, episodes []importedEpisode) (int, error) {
	if s.seriesDrops == nil || s.nextUp == nil {
		slog.InfoContext(ctx, "history import: continue watching pass skipped: stores not configured", "component", "historyimport")
		return 0, nil
	}
	// One summary line says where the pass stopped and why: how many shows
	// the run imported, how many the source's row lists, how many Silo would
	// surface among the rest, and how many of those were dropped or kept.
	var stats struct{ shows, listed, surfaced, keptNewer, keptDropped, dropped int }
	defer func() {
		slog.InfoContext(ctx, "history import: continue watching pass",
			"component", "historyimport", "profile_id", profileID,
			"episodes", len(episodes), "row_shows", len(row.SourceSeriesIDs), "row_series", len(row.Series),
			"shows", stats.shows, "listed", stats.listed, "surfaced_unlisted", stats.surfaced,
			"kept_newer_activity", stats.keptNewer, "kept_existing_drop", stats.keptDropped, "dropped", stats.dropped)
	}()
	if len(episodes) == 0 {
		return 0, nil
	}

	// The shows the run imported, with each one's newest imported stamp.
	// Episodes of one source series share a show, so one is resolved per
	// source series once it resolves.
	anchors := make(map[string]time.Time)
	bySource := make(map[string]string)
	for _, episode := range episodes {
		seriesID, known := bySource[episode.sourceSeriesID]
		if !known || episode.sourceSeriesID == "" {
			id, ok, err := s.seriesDrops.ResolveDropSeries(ctx, episode.itemID)
			if err != nil {
				return 0, err
			}
			if !ok {
				continue
			}
			seriesID = id
			if episode.sourceSeriesID != "" {
				bySource[episode.sourceSeriesID] = id
			}
		}
		// Postgres keeps microseconds, so compare stamps as stored.
		at := episode.at.UTC().Truncate(time.Microsecond)
		if current, ok := anchors[seriesID]; !ok || at.After(current) {
			anchors[seriesID] = at
		}
	}
	stats.shows = len(anchors)
	if len(anchors) == 0 {
		return 0, nil
	}

	listed := make(map[string]bool)
	for sourceSeriesID := range row.SourceSeriesIDs {
		if seriesID, ok := bySource[sourceSeriesID]; ok {
			listed[seriesID] = true
		}
	}
	for _, series := range row.Series {
		match, _, err := s.matcher.Match(ctx, series)
		if err != nil {
			return 0, err
		}
		if match != nil {
			listed[match.MediaItemID] = true
		}
	}

	// Only shows Silo would surface are dropped: a finished show isn't in
	// Continue Watching either way, and dropping it would hide its next
	// season, which the source would show.
	candidates := make([]string, 0)
	for seriesID := range anchors {
		if listed[seriesID] {
			stats.listed++
			continue
		}
		surfaced, err := s.nextUp.ListNextUp(ctx, catalog.NextUpQuery{
			UserID: userID, ProfileID: profileID, SeriesID: seriesID, Limit: 1, EnableResumable: true,
		})
		if err != nil {
			return 0, err
		}
		if len(surfaced) > 0 {
			candidates = append(candidates, seriesID)
		}
	}
	stats.surfaced = len(candidates)
	if len(candidates) == 0 {
		return 0, nil
	}
	slices.Sort(candidates)

	activity, err := s.seriesDrops.LatestActivity(ctx, userID, profileID, candidates)
	if err != nil {
		return 0, err
	}
	existingDrops, err := s.seriesDrops.ListDropped(ctx, userID, profileID, candidates)
	if err != nil {
		return 0, err
	}
	existing := make(map[string]catalog.DroppedSeries, len(existingDrops))
	for _, drop := range existingDrops {
		existing[drop.SeriesID] = drop
	}

	dropped := 0
	for _, seriesID := range candidates {
		anchor := anchors[seriesID]
		// Playback newer than anything imported means the user is watching
		// the show in Silo, whatever the source's row says.
		if latest, ok := activity[seriesID]; ok && latest.After(anchor) {
			stats.keptNewer++
			continue
		}
		var observed *time.Time
		if drop, ok := existing[seriesID]; ok {
			// Already hidden, or dropped since and watched again: the
			// profile's own choice stands.
			if drop.Active || !drop.DroppedAt.Before(anchor) {
				stats.keptDropped++
				continue
			}
			droppedAt := drop.DroppedAt
			observed = &droppedAt
		}
		applied, err := s.seriesDrops.ImportDrop(ctx, userID, profileID, seriesID, anchor, observed)
		if err != nil {
			return dropped, err
		}
		if applied {
			dropped++
			stats.dropped = dropped
		}
	}
	return dropped, nil
}
