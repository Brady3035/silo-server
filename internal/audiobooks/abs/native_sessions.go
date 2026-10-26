package abs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/clientip"
	"github.com/Silo-Server/silo-server/internal/models"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// PlaybackSessionManager is the native Silo playback-session surface ABS needs
// to make compatibility playback visible to live admin/session monitoring.
type PlaybackSessionManager interface {
	StartSessionWithFilesContext(ctx context.Context, userID int, profileID string, effectiveFileID int, requestedFileID int, method playback.PlayMethod, transcodeAudio bool) (*playback.Session, error)
	UpdateProgress(sessionID string, position float64, isPaused bool) error
	UpdateProgressListened(sessionID string, position, listened float64) error
	UpdateStreamState(sessionID string, state playback.SessionStreamState) error
	BeginTransport(sessionID string) error
	EndTransport(sessionID string) error
	SetProgressPersistenceDisabled(sessionID string, disabled bool) error
	// SetTimelineDuration records the whole book's length, since ABS reports
	// positions in the book rather than in the session's first file.
	SetTimelineDuration(sessionID string, seconds float64) error
	// FinishSession ends the play and runs the manager's finish hooks, which
	// write the admin playback history row.
	FinishSession(ctx context.Context, sessionID string) error
	GetSession(sessionID string) (*playback.Session, error)
	// RegisterReconstructed adds a session under a caller-chosen ID, or
	// returns the live one already under it.
	RegisterReconstructed(s *playback.Session) *playback.Session
}

const (
	// reportedPlaybackGrace is how long after its last report a play that a
	// client only reports through /session/local is over. Such clients send
	// no stop, so silence is the only end there is.
	reportedPlaybackGrace = 90 * time.Second
	// liveReportWindow separates live playback from catch-up: a session last
	// updated longer ago than this is offline listening synced late, or a
	// client re-sending an old session, not a play happening now.
	liveReportWindow = 2 * time.Minute
)

// reportedPlaybackNamespace derives the native session ID of a play a client
// reports through /session/local from the client's own session ID.
var reportedPlaybackNamespace = uuid.MustParse("3f6c1a2e-8b4d-4e7a-9c15-6d2f0b8e7a41")

// PlaybackSessionSyncer flushes the local native-session snapshot into the
// shared admin live-session table.
type PlaybackSessionSyncer interface {
	SyncNow(ctx context.Context) error
}

func (h *Handler) startNativePlaybackSession(
	r *http.Request,
	a ctxAuth,
	files []*models.MediaFile,
	startPosition float64,
	device absDeviceInfo,
) (*playback.Session, error) {
	if h == nil || h.deps.NativeSessions == nil || len(files) == 0 {
		return nil, nil
	}
	userID, err := strconv.Atoi(a.UserID)
	if err != nil {
		return nil, fmt.Errorf("invalid ABS user id %q: %w", a.UserID, err)
	}

	file := files[0]
	sessionCtx := playback.WithClientInfo(r.Context(), absPlaybackClientInfo(r, device))
	session, err := h.deps.NativeSessions.StartSessionWithFilesContext(
		sessionCtx,
		userID,
		a.ProfileID,
		file.ID,
		file.ID,
		playback.PlayDirect,
		false,
	)
	if err != nil {
		return nil, err
	}

	if err := h.deps.NativeSessions.SetProgressPersistenceDisabled(session.ID, true); err != nil {
		slog.WarnContext(r.Context(), "abs play: disable native progress persistence failed", "component", "audiobooks",
			"session_id", session.ID, "error", err)
	} else {
		session.DisableProgressPersistence = true
	}

	var bookDuration float64
	for _, f := range files {
		bookDuration += float64(f.Duration)
	}
	if err := h.deps.NativeSessions.SetTimelineDuration(session.ID, bookDuration); err != nil {
		slog.WarnContext(r.Context(), "abs play: set native session book duration failed", "component", "audiobooks",
			"session_id", session.ID, "error", err)
	} else {
		session.TimelineDuration = bookDuration
	}

	if startPosition > 0 {
		if err := h.deps.NativeSessions.UpdateProgress(session.ID, startPosition, false); err != nil {
			slog.WarnContext(r.Context(), "abs play: seed native session progress failed", "component", "audiobooks",
				"session_id", session.ID, "position", startPosition, "error", err)
		} else {
			session.Position = startPosition
			session.IsPaused = false
		}
	}

	streamBitrateKbps := 0
	if file.Bitrate > 0 {
		streamBitrateKbps = file.Bitrate
	}
	if err := h.deps.NativeSessions.UpdateStreamState(session.ID, playback.SessionStreamState{
		PlayMethod:        playback.PlayDirect,
		BasePlayMethod:    playback.PlayDirect,
		ClientIP:          requestClientIP(r),
		ClientName:        session.ClientName,
		ClientVersion:     session.ClientVersion,
		ClientUserAgent:   session.ClientUserAgent,
		StreamBitrateKbps: streamBitrateKbps,
	}); err != nil {
		slog.WarnContext(r.Context(), "abs play: update native session stream state failed", "component", "audiobooks",
			"session_id", session.ID, "error", err)
	} else {
		session.PlayMethod = playback.PlayDirect
		session.BasePlayMethod = playback.PlayDirect
		session.ClientIP = requestClientIP(r)
		session.StreamBitrateKbps = streamBitrateKbps
	}

	h.syncNativeSessionsNow(r.Context(), "abs_start")
	return session, nil
}

// updateNativePlaybackProgress mirrors a session sync. Its timeListened is the
// time actually listened since the previous sync, so it is what the native
// session credits as listening time; the position may have jumped by a seek.
func (h *Handler) updateNativePlaybackProgress(ctx context.Context, sessionID string, position, listened float64) {
	if h == nil || h.deps.NativeSessions == nil || sessionID == "" {
		return
	}
	if err := h.deps.NativeSessions.UpdateProgressListened(sessionID, position, listened); err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.WarnContext(ctx, "abs session sync: update native session progress failed", "component", "audiobooks",
				"session_id", sessionID, "position", position, "error", err)
		}
		return
	}
	h.syncNativeSessionsNow(ctx, "abs_progress")
}

// trackReportedPlayback mirrors live playback that a client reports only
// through /session/local or /session/local-all into a native playback
// session, as handlePlayStart does for a client that opens a session. Some
// clients (Plappa and Prologue on iOS) never call /play: they stream the files
// directly and post their local session every few seconds while playing.
//
// The native session ID is derived from the account, profile and client
// session ID, so every report of one play, on any replica and after an idle
// expiry, lands on the same session and the same admin history row. A report
// never fails because of this: the client is playing whether or not Silo
// shows it.
func (h *Handler) trackReportedPlayback(r *http.Request, a ctxAuth, access catalog.AccessFilter, sess localPlaybackSession) {
	if h == nil || h.deps.NativeSessions == nil || sess.ID == "" || !sess.isLive(time.Now()) {
		return
	}
	userID, err := strconv.Atoi(a.UserID)
	if err != nil {
		return
	}
	ctx := r.Context()
	id := uuid.NewSHA1(reportedPlaybackNamespace, []byte(a.UserID+"\x00"+a.ProfileID+"\x00"+sess.ID)).String()
	if _, err := h.deps.NativeSessions.GetSession(id); err == nil {
		if err := h.deps.NativeSessions.UpdateProgress(id, sess.CurrentTime, false); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
			slog.WarnContext(ctx, "abs local session sync: update native session progress failed", "component", "audiobooks",
				"session_id", id, "error", err)
		}
		return
	}

	files, err := h.deps.MediaStore.GetMediaFiles(ctx, sess.LibraryItemID, access)
	if err != nil || len(files) == 0 {
		if err != nil {
			slog.WarnContext(ctx, "abs local session sync: load files for native session failed", "component", "audiobooks",
				"library_item_id", sess.LibraryItemID, "error", err)
		}
		return
	}
	var bookDuration float64
	for _, f := range files {
		bookDuration += float64(f.Duration)
	}
	client := absPlaybackClientInfo(r, sess.DeviceInfo).Normalized()
	file := files[0]
	// Measure from where the client says the play began, so the stretch
	// before its first report counts; the watch-time seek check still judges
	// it, so a resume that jumped is not credited.
	var watch playback.WatchTime
	if started := time.UnixMilli(int64(sess.StartedAt)); sess.StartedAt > 0 && started.Before(time.Now()) {
		watch.Observe(sess.StartTime, false, started)
	}
	h.deps.NativeSessions.RegisterReconstructed(&playback.Session{
		ID:                         id,
		UserID:                     userID,
		ProfileID:                  a.ProfileID,
		MediaFileID:                file.ID,
		RequestedMediaFileID:       file.ID,
		PlayMethod:                 playback.PlayDirect,
		BasePlayMethod:             playback.PlayDirect,
		ClientIP:                   requestClientIP(r),
		ClientName:                 client.Name,
		ClientVersion:              client.Version,
		ClientUserAgent:            client.UserAgent,
		StreamBitrateKbps:          max(file.Bitrate, 0),
		DisableProgressPersistence: true,
		TimelineDuration:           bookDuration,
		IdleGrace:                  reportedPlaybackGrace,
		Watch:                      watch,
	})
	if err := h.deps.NativeSessions.UpdateProgress(id, sess.CurrentTime, false); err != nil {
		slog.WarnContext(ctx, "abs local session sync: seed native session progress failed", "component", "audiobooks",
			"session_id", id, "error", err)
	}
	h.syncNativeSessionsNow(ctx, "abs_local_start")
}

func (h *Handler) stopNativePlaybackSession(ctx context.Context, sessionID string) {
	if h == nil || h.deps.NativeSessions == nil || sessionID == "" {
		return
	}
	if err := h.deps.NativeSessions.FinishSession(ctx, sessionID); err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.WarnContext(ctx, "abs session close: finish native session failed", "component", "audiobooks",
				"session_id", sessionID, "error", err)
		}
		return
	}
	h.syncNativeSessionsNow(ctx, "abs_close")
}

func (h *Handler) beginNativePlaybackTransport(sessionID string) bool {
	if h == nil || h.deps.NativeSessions == nil || sessionID == "" {
		return false
	}
	if err := h.deps.NativeSessions.BeginTransport(sessionID); err != nil {
		if !errors.Is(err, playback.ErrSessionNotFound) {
			slog.Warn("abs public track: begin native transport failed",
				"session_id", sessionID, "error", err)
		}
		return false
	}
	return true
}

func (h *Handler) endNativePlaybackTransport(sessionID string) {
	if h == nil || h.deps.NativeSessions == nil || sessionID == "" {
		return
	}
	if err := h.deps.NativeSessions.EndTransport(sessionID); err != nil && !errors.Is(err, playback.ErrSessionNotFound) {
		slog.Warn("abs public track: end native transport failed",
			"session_id", sessionID, "error", err)
	}
}

func (h *Handler) syncNativeSessionsNow(ctx context.Context, reason string) {
	if h == nil || h.deps.NativeSessionSyncer == nil {
		return
	}
	if err := h.deps.NativeSessionSyncer.SyncNow(ctx); err != nil {
		slog.ErrorContext(ctx, "abs: failed to sync native playback sessions", "component", "audiobooks", "reason", reason, "error", err)
	}
}

func writeNativePlaybackStartError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, playback.ErrTooManyStreams):
		http.Error(w, "too many concurrent streams", http.StatusTooManyRequests)
	case errors.Is(err, playback.ErrTooManyTranscodes):
		http.Error(w, "too many concurrent transcodes", http.StatusTooManyRequests)
	case errors.Is(err, playback.ErrTranscodingDisabled):
		http.Error(w, "transcoding is disabled for your user", http.StatusForbidden)
	case errors.Is(err, playback.ErrAudioTranscodingDisabled):
		http.Error(w, "audio transcoding is disabled for your user", http.StatusForbidden)
	default:
		slog.Error("abs play: start native playback session failed", "error", err)
		http.Error(w, "failed to start playback session", http.StatusInternalServerError)
	}
}

// absDeviceInfo is the part of the deviceInfo an Audiobookshelf client sends
// with /play and with its local sessions that names the client.
type absDeviceInfo struct {
	ClientName    string `json:"clientName"`
	ClientVersion string `json:"clientVersion"`
}

// absPlaybackClientInfo names the client of a play: the deviceInfo the client
// sent, else the request headers.
func absPlaybackClientInfo(r *http.Request, device absDeviceInfo) playback.ClientInfo {
	client := absPlaybackClientInfoFromRequest(r)
	if name := strings.TrimSpace(device.ClientName); name != "" {
		client.Name, client.Version = name, strings.TrimSpace(device.ClientVersion)
	}
	return client
}

func absPlaybackClientInfoFromRequest(r *http.Request) playback.ClientInfo {
	if r == nil {
		return playback.ClientInfo{Name: "Audiobookshelf"}
	}
	name := firstHeaderValue(r,
		"X-Silo-Client",
		"X-Client-Name",
		"X-Device-Name",
		"X-Emby-Client",
	)
	if name == "" {
		name = "Audiobookshelf"
	}
	return playback.ClientInfo{
		Name: name,
		Version: firstHeaderValue(r,
			"X-Silo-Client-Version",
			"X-Client-Version",
			"X-Emby-Client-Version",
		),
		UserAgent: r.UserAgent(),
	}
}

func firstHeaderValue(r *http.Request, names ...string) string {
	for _, name := range names {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

func requestClientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if ip := clientip.FromContext(r.Context()); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return strings.TrimSpace(r.RemoteAddr)
}
