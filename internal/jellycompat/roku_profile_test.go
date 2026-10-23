package jellycompat

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/models"
)

// rokuProfile is the DeviceProfile Moonfin for Roku (a jellyfin-roku fork)
// builds in source/utils/deviceCapabilities.bs for a 4K Roku on stereo output.
// Its video transcoding profiles list AC-3 but not AAC, and its AAC codec
// profile requires AudioProfile != Main.
func rokuProfile(t *testing.T) DeviceProfile {
	t.Helper()
	data, err := os.ReadFile("testdata/roku_device_profile.json")
	if err != nil {
		t.Fatal(err)
	}
	var profile DeviceProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func rokuTestVersion(audio models.AudioTrack) catalog.FileVersion {
	return catalog.FileVersion{
		FileID: 7, Container: "mkv", CodecVideo: "h264", CodecAudio: audio.Codec, Resolution: "1080p", Bitrate: 8000, Duration: 3600,
		VideoTracks: []models.VideoTrack{{Codec: "h264", Profile: "High", Level: 41, BitDepth: 8, Width: 1920, Height: 1080, VideoRangeType: "SDR", Bitrate: 7_500_000}},
		AudioTracks: []models.AudioTrack{audio},
	}
}

func TestRokuProfileNegotiatesAC3WhenAACIsNotListed(t *testing.T) {
	h := &PlaybackHandler{codec: NewResourceIDCodec()}
	req := playbackInfoRequest{MaxStreamingBitrate: 140_000_000}
	withAAC := rokuProfile(t)
	for i := range withAAC.TranscodingProfiles {
		if withAAC.TranscodingProfiles[i].Type == "Video" {
			withAAC.TranscodingProfiles[i].AudioCodec = "aac," + withAAC.TranscodingProfiles[i].AudioCodec
		}
	}
	for _, tc := range []struct {
		name          string
		profile       DeviceProfile
		allowAC3      bool
		audio         models.AudioTrack
		wantDirect    bool
		wantTranscode bool
		wantRemux     bool
		wantCodec     string
		wantSegment   string
	}{
		// The fallback a direct-playable file advertises is an AC-3 encode too.
		{name: "AAC-LC direct plays", profile: rokuProfile(t), allowAC3: true, audio: models.AudioTrack{Codec: "aac", Profile: "LC", Channels: 2, Default: true}, wantDirect: true, wantTranscode: true, wantCodec: compatAudioCodecAC3, wantSegment: compatAudioCodecV1PathSegment},
		{name: "DTS remuxes with AC-3", profile: rokuProfile(t), allowAC3: true, audio: models.AudioTrack{Codec: "dts", Channels: 6, Default: true}, wantTranscode: true, wantCodec: compatAudioCodecAC3, wantSegment: compatAudioCodecV1PathSegment, wantRemux: true},
		{name: "TrueHD 7.1 remuxes with AC-3", profile: rokuProfile(t), allowAC3: true, audio: models.AudioTrack{Codec: "truehd", Channels: 8, Default: true}, wantTranscode: true, wantCodec: compatAudioCodecAC3, wantSegment: compatAudioCodecV1PathSegment, wantRemux: true},
		{name: "surround AC-3 on stereo output re-encodes", profile: rokuProfile(t), allowAC3: true, audio: models.AudioTrack{Codec: "ac3", Channels: 6, Default: true}, wantTranscode: true, wantCodec: compatAudioCodecAC3, wantSegment: compatAudioCodecV1PathSegment, wantRemux: true},
		{name: "no AC-3 executor offers nothing", profile: rokuProfile(t), audio: models.AudioTrack{Codec: "dts", Channels: 6, Default: true}},
		// The AAC encode states its LC profile, so Roku's AudioProfile != Main
		// rule passes and a profile that lists AAC keeps getting AAC.
		{name: "AAC stays preferred when listed", profile: withAAC, allowAC3: true, audio: models.AudioTrack{Codec: "dts", Channels: 6, Default: true}, wantTranscode: true, wantSegment: compatAudioV2PathSegment, wantRemux: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := tc.profile
			profile.allowAC3Output = tc.allowAC3
			source := h.buildPlaybackSource("item", "play", rokuTestVersion(tc.audio), profile, req, true)
			if source.SupportsDirectPlay != tc.wantDirect || source.SupportsTranscoding != tc.wantTranscode {
				t.Fatalf("direct=%v transcoding=%v, want direct=%v transcoding=%v", source.SupportsDirectPlay, source.SupportsTranscoding, tc.wantDirect, tc.wantTranscode)
			}
			if source.TargetAudioCodec != tc.wantCodec {
				t.Fatalf("TargetAudioCodec = %q, want %q", source.TargetAudioCodec, tc.wantCodec)
			}
			if tc.wantTranscode {
				if source.HLSRemux != tc.wantRemux || source.TranscodeAudio != tc.wantRemux {
					t.Fatalf("HLSRemux=%v TranscodeAudio=%v, want video-copy audio encode %v", source.HLSRemux, source.TranscodeAudio, tc.wantRemux)
				}
				if got := compatHLSRoutePathSegment(source); got != tc.wantSegment {
					t.Fatalf("route segment = %q, want %q", got, tc.wantSegment)
				}
			}
		})
	}
}

func rokuAC3Source(t *testing.T) PlaybackMediaSource {
	t.Helper()
	source := testCompatSource(NewResourceIDCodec(), testCompatVersion())
	source.ID = "source"
	source.HLSRemux, source.TranscodeAudio = true, true
	source.TargetAudioCodec = compatAudioCodecAC3
	return source
}

func TestAC3PlaybackURLsRejectOlderAPIRouters(t *testing.T) {
	legacy := chi.NewRouter()
	reached := false
	for _, prefix := range []string{"", "/audio-v2", "/remux-v1", "/remux-ts-v1", "/remux-dv-v1", "/hevc-v1"} {
		for _, suffix := range []string{"/master.m3u8", "/hls/{playlistId}/stream.m3u8", "/hls/{playlistId}/{segmentId}.{segmentContainer}"} {
			legacy.Get("/Videos/{id}"+prefix+suffix, func(w http.ResponseWriter, _ *http.Request) {
				reached = true
				w.WriteHeader(http.StatusOK)
			})
		}
	}
	remux := rokuAC3Source(t)
	encode := rokuAC3Source(t)
	encode.HLSRemux, encode.TranscodeAudio, encode.TargetVideoCodec = false, false, compatVideoCodecHEVC
	for _, source := range []PlaybackMediaSource{remux, encode} {
		dto := (&PlaybackHandler{}).mediaSourceDTO("item", "play", "token", source)
		urls := []string{dto.TranscodingURL}
		for _, resource := range []string{"stream.m3u8", "init.mp4", "seg_00001.m4s"} {
			urls = append(urls, buildSegmentProxyPath("item", "play", source.ID, resource, compatHLSRoutePathSegment(source)))
		}
		for _, resourceURL := range urls {
			reached = false
			rec := httptest.NewRecorder()
			legacy.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, resourceURL, nil))
			if rec.Code != http.StatusNotFound || reached || !strings.Contains(resourceURL, "/audio-codec-v1/") {
				t.Errorf("AC-3 URL %q reached an older AAC handler (status %d) or lacks its route", resourceURL, rec.Code)
			}
		}
	}
}

func TestAC3HLSRoutesEnforceNegotiatedRecipe(t *testing.T) {
	cfg, err := config.LoadFromDB(map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	aac := rokuAC3Source(t)
	aac.TargetAudioCodec = ""
	for _, test := range []struct {
		name   string
		source PlaybackMediaSource
		prefix string
	}{
		{name: "AAC route rejects AC-3", source: rokuAC3Source(t), prefix: "/audio-v2"},
		{name: "legacy route rejects AC-3", source: rokuAC3Source(t)},
		{name: "AC-3 route rejects AAC", source: aac, prefix: "/audio-codec-v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewPlaybackSessionStore(time.Hour, nil)
			store.Put(PlaybackSession{
				ID: "play", CompatToken: "token", RouteItemID: "item", UpstreamSessionID: "upstream",
				MediaSources: []PlaybackMediaSource{test.source},
			})
			sessions := NewSessionStore(time.Hour, nil)
			if err := sessions.Put(Session{Token: "token", StreamAppUserID: 1}); err != nil {
				t.Fatal(err)
			}
			router := NewRouter(Dependencies{Config: cfg, SessionStore: sessions, PlaybackStore: store})
			for _, suffix := range []string{"/master.m3u8", "/hls/play/stream.m3u8", "/hls/play/init.mp4", "/hls/play/seg_00001.m4s"} {
				req := httptest.NewRequest(http.MethodGet, "/Videos/item"+test.prefix+suffix+"?PlaySessionId=play&MediaSourceId=source", nil)
				req.Header.Set("X-Emby-Token", "token")
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				if rec.Code != http.StatusNotFound {
					t.Fatalf("%s status = %d, want 404 before starting playback: %s", suffix, rec.Code, rec.Body.String())
				}
			}
			stored, _ := store.Get("play")
			if stored == nil || stored.TranscodeStarted || stored.Recipe != nil || stored.UpstreamSessionID != "upstream" {
				t.Fatalf("mismatched route changed the session: %+v", stored)
			}
		})
	}
}
