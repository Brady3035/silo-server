package jellycompat

import (
	"context"
	"fmt"
	"strings"

	"github.com/Silo-Server/silo-server/internal/nodepool"
	"github.com/Silo-Server/silo-server/internal/noderouting"
	"github.com/Silo-Server/silo-server/internal/playback"
)

// Some clients, Roku among them, list AC-3 but not AAC in their video
// transcoding profiles. PlaybackInfo then negotiates an AC-3 audio encode
// (see DeviceProfile.outputAudioCodecs), but only while an executor that the
// routing policy allows advertises audio_to_ac3. Older binaries do not, and
// would encode AAC from the same recipe.

// compatSourceEncodesAC3 reports whether source's HLS bytes carry an AC-3
// encode.
func compatSourceEncodesAC3(source PlaybackMediaSource) bool {
	return compatHLSTranscodesAudio(source) && compatSourceTargetAudioCodec(source) == compatAudioCodecAC3
}

func compatSupportsAC3Encoding(transformations []playback.TransformationV3) bool {
	for _, transformation := range transformations {
		if strings.EqualFold(strings.TrimSpace(transformation.Name), playback.TransformationAudioToAC3V3) &&
			strings.EqualFold(strings.TrimSpace(transformation.Executor), playback.ExecutorServerV3) &&
			strings.TrimSpace(transformation.RecipeVersion) == playback.TransformationAudioToAC3RecipeVersionV3 {
			return true
		}
	}
	return false
}

func (h *PlaybackHandler) compatLocalAC3Available(ctx context.Context) bool {
	registry, err := h.localAudioTransformationRegistry(ctx)
	return err == nil && registry.Available(playback.TransformationAudioToAC3V3)
}

func compatNodeSupportsAC3(node *nodepool.Node) bool {
	if node == nil {
		return false
	}
	info, ok := compatNodeReport(node)
	return ok && compatSupportsAC3Encoding(info.Transformations)
}

// compatAC3Executable reports whether some executor the routing policy allows
// for an HLS remux or video encode can run audio_to_ac3. Stored node reports
// keep negotiation independent of slow capability requests.
func (h *PlaybackHandler) compatAC3Executable(ctx context.Context) bool {
	localChecked, localAvailable := false, false
	for _, request := range []noderouting.Request{
		{Workload: noderouting.WorkloadRemux, Delivery: noderouting.DeliveryHLSRemux},
		{Workload: noderouting.WorkloadVideoTranscode, Delivery: noderouting.DeliveryHLSVideo},
	} {
		request.Policy, request.ProxyAllowed = h.playbackRoutingPolicy(), h.JWTSecret != ""
		compiled, err := noderouting.Candidates(request)
		if err != nil {
			continue
		}
		for _, shape := range compiled.Candidates {
			switch shape.Execution {
			case noderouting.ExecutionAPI:
				if !localChecked {
					localChecked, localAvailable = true, h.compatLocalAC3Available(ctx)
				}
				if localAvailable {
					return true
				}
			case noderouting.ExecutionTranscode:
				if h.compatAnyTranscodeNode(compatNodeSupportsAC3) {
					return true
				}
			}
		}
	}
	return false
}

// compatAnyTranscodeNode reports whether a pooled transcode node that takes
// new work satisfies supports.
func (h *PlaybackHandler) compatAnyTranscodeNode(supports func(*nodepool.Node) bool) bool {
	enumerator, canList := h.NodePlanner.(compatTranscodeNodeEnumerator)
	lookup, canLookup := h.NodePlanner.(compatTranscodeNodeLookup)
	if !canList || !canLookup {
		return false
	}
	for _, nodeURL := range enumerator.TranscodeNodeURLs() {
		if node, ok := lookup.TranscodeNodeByURL(nodeURL); ok && node.TakesNewWork() && supports(node) {
			return true
		}
	}
	return false
}

// compatAC3Routing keeps an AC-3 recipe on executors that advertise it,
// excluding the API host's shape when its FFmpeg cannot.
func (h *PlaybackHandler) compatAC3Routing(
	ctx context.Context,
	videoTranscode bool,
	eligible func(*nodepool.Node) bool,
	excludedShapes map[string]struct{},
) (func(*nodepool.Node) bool, map[string]struct{}) {
	baseEligible := eligible
	eligible = func(node *nodepool.Node) bool {
		return compatNodeSupportsAC3(node) && (baseEligible == nil || baseEligible(node))
	}
	if !h.compatLocalAC3Available(ctx) {
		excluded := make(map[string]struct{}, len(excludedShapes)+1)
		for id := range excludedShapes {
			excluded[id] = struct{}{}
		}
		if videoTranscode {
			excluded["hls_video_api"] = struct{}{}
		} else {
			excluded[noderouting.ShapeHLSRemuxAPI] = struct{}{}
		}
		excludedShapes = excluded
	}
	return eligible, excludedShapes
}

// requireLocalAC3Capability refuses to start an AC-3 recipe on an API host
// whose FFmpeg does not advertise it.
func (h *PlaybackHandler) requireLocalAC3Capability(ctx context.Context, source PlaybackMediaSource) error {
	if !compatSourceEncodesAC3(source) || h.compatLocalAC3Available(ctx) {
		return nil
	}
	return fmt.Errorf("%w: local FFmpeg does not support %s recipe %s",
		errAudioDownmixCapabilityUnavailable, playback.TransformationAudioToAC3V3, playback.TransformationAudioToAC3RecipeVersionV3)
}
