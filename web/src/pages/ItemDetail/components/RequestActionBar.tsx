import { useState } from "react";
import {
  Ban,
  Bell,
  BellOff,
  Bookmark,
  BookmarkCheck,
  CircleCheck,
  Clock,
  Hourglass,
  Library,
  type LucideIcon,
  Plus,
  X,
} from "lucide-react";
import type { MediaRequest, RequestMediaDetail, RequestState } from "@/api/types";
import { CancelRequestDialog } from "@/components/CancelRequestDialog";
import { RequestDownloadProgress } from "@/components/RequestDownloadProgress";
import { RequestSeasonsDialog } from "@/components/RequestSeasonsDialog";
import {
  useCancelMediaRequest,
  useCreateMediaRequest,
  useMediaRequest,
  useMyMediaRequests,
  useRequestFeatureStatus,
  useToggleRequestFollow,
} from "@/hooks/queries/useRequests";
import { useToggleWatchlistTitle } from "@/hooks/queries/watchlistTitles";
import { useAuth } from "@/hooks/useAuth";
import { useViewTransitionNavigate } from "@/hooks/useViewTransition";
import ViewTransitionLink from "@/components/ViewTransitionLink";
import {
  canCancelOwnRequest,
  formatRequestDisplayState,
  formatRequestReason,
  requestDisplayState,
  requestInputFromMediaResult,
  type RequestDisplayState,
} from "@/lib/mediaRequests";
import { watchlistTitlesAvailable } from "@/lib/watchlistTitles";
import ActionBar, {
  type ActionBarLink,
  type ActionBarPrimaryAction,
  type ActionBarSecondaryAction,
} from "./ActionBar";

const STATE_ICONS: Record<RequestDisplayState, LucideIcon> = {
  pending: Clock,
  approved: CircleCheck,
  processing: Hourglass,
  partially_available: Library,
  available: Library,
  declined: Ban,
  cancelled: Ban,
  failed: Ban,
};

interface RequestActionBarProps {
  item: RequestMediaDetail;
  /**
   * Opens the library's copy of the title. Set only when the page could not
   * confirm whether the viewer may open it; a copy they cannot open reads as
   * a disabled "In the library", with no link.
   */
  libraryHref?: string;
}

/**
 * The action row for a title outside the library: Request in the Play pill's
 * place, the request's state once there is one, cancelling or following that
 * request, and how far its download is. Owns the request mutations so their
 * pending state re-renders only this row, as MediaUserActionBar does for
 * library items.
 */
export default function RequestActionBar({ item, libraryHref }: RequestActionBarProps) {
  const navigate = useViewTransitionNavigate();
  const createRequest = useCreateMediaRequest();
  const cancelRequest = useCancelMediaRequest();
  const toggleFollow = useToggleRequestFollow();
  const toggleWatchlist = useToggleWatchlistTitle();
  const featureStatus = useRequestFeatureStatus();
  const ownRequest = useOwnCancellableRequest(item, item.request, false);
  const own4KRequest = useOwnCancellableRequest(item, item.request_4k, true);
  // Which of the viewer's requests the cancel dialog is for.
  const [confirmCancel, setConfirmCancel] = useState<MediaRequest | null>(null);
  const [pickSeasons, setPickSeasons] = useState(false);
  // A series TMDB lists seasons for is requested season by season.
  const seasonPicker = item.media_type === "series" && (item.seasons?.length ?? 0) > 0;

  const state = requestDisplayState(item.request.status, undefined, item.request.state);
  // The title's 4K request is separate from the regular one: a title in the
  // library in HD can still be requested in 4K.
  const uhd = item.request_4k;
  const uhdState = uhd ? requestDisplayState(uhd.status, undefined, uhd.state) : undefined;
  const UHDStateIcon = uhdState ? STATE_ICONS[uhdState] : undefined;
  const inLibrary =
    item.availability === "available" || state === "available" || Boolean(item.library_content_id);
  const following = item.request.following === true;
  // Someone else's open request: the viewer can ask to hear when it lands
  // instead of requesting the title again.
  const canFollow =
    state !== undefined &&
    state !== "available" &&
    item.request.reason === "already_requested" &&
    !item.request.requested_by_viewer;

  let primaryAction: ActionBarPrimaryAction;
  if (item.request.requestable) {
    primaryAction = {
      label: item.media_type === "series" ? "Request series" : "Request movie",
      icon: Plus,
      pending: createRequest.isPending,
      onClick: seasonPicker
        ? () => setPickSeasons(true)
        : () => createRequest.mutate(requestInputFromMediaResult(item)),
    };
  } else if (inLibrary) {
    primaryAction = libraryHref
      ? { label: "Open in library", icon: Library, onClick: () => navigate(libraryHref) }
      : { label: "In the library", icon: Library, disabled: true };
  } else if (state) {
    primaryAction = {
      // The viewer's side of a pending request: they asked, nobody has answered.
      label: state === "pending" ? "Requested" : formatRequestDisplayState(state),
      icon: STATE_ICONS[state],
      disabled: true,
    };
  } else {
    primaryAction = { label: formatRequestReason(item.request.reason), icon: Ban, disabled: true };
  }

  const watchlistSupported = watchlistTitlesAvailable(featureStatus.data);
  const inWatchlist = item.in_watchlist === true;
  // The server requests a title added to the watchlist only while it has no
  // request and the viewer may make one; say so before the add, not after.
  const addAlsoRequests =
    watchlistSupported &&
    featureStatus.data?.watchlist_requests === true &&
    !inWatchlist &&
    !inLibrary &&
    item.request.requestable;

  const secondaryActions: ActionBarSecondaryAction[] = [];
  if (watchlistSupported) {
    secondaryActions.push({
      id: "watchlist",
      label: inWatchlist ? "On Watchlist" : "Add to Watchlist",
      icon: inWatchlist ? BookmarkCheck : Bookmark,
      pressed: inWatchlist,
      pending: toggleWatchlist.isPending,
      onClick: () =>
        toggleWatchlist.mutate({
          mediaType: item.media_type,
          tmdbID: item.tmdb_id,
          title: item.title,
          inWatchlist,
          request: item.request,
        }),
    });
  }
  if (uhd?.requestable) {
    secondaryActions.push({
      id: "request-4k",
      label: "Request in 4K",
      icon: Plus,
      pending: createRequest.isPending,
      onClick: () => createRequest.mutate({ ...requestInputFromMediaResult(item), is_4k: true }),
    });
  }
  if (ownRequest) {
    secondaryActions.push({
      id: "cancel",
      label: "Cancel request",
      icon: X,
      pending: cancelRequest.isPending,
      onClick: () => setConfirmCancel(ownRequest),
    });
  }
  if (own4KRequest) {
    secondaryActions.push({
      id: "cancel-4k",
      label: "Cancel 4K request",
      icon: X,
      pending: cancelRequest.isPending,
      onClick: () => setConfirmCancel(own4KRequest),
    });
  }
  if (canFollow) {
    secondaryActions.push({
      id: "follow",
      label: following ? "Stop notifying me" : "Notify me when available",
      icon: following ? BellOff : Bell,
      pressed: following,
      pending: toggleFollow.isPending,
      onClick: () =>
        toggleFollow.mutate({
          mediaType: item.media_type,
          tmdbID: item.tmdb_id,
          follow: !following,
        }),
    });
  }

  const links: ActionBarLink[] = [];
  if (item.imdb_id) {
    links.push({ label: "IMDb", href: `https://www.imdb.com/title/${item.imdb_id}` });
  }
  links.push({
    label: "TMDB",
    href: `https://www.themoviedb.org/${item.media_type === "series" ? "tv" : "movie"}/${item.tmdb_id}`,
  });

  return (
    <>
      <ActionBar primaryAction={primaryAction} secondaryActions={secondaryActions} links={links} />
      {addAlsoRequests ? (
        <p className="text-muted-foreground mt-2.5 text-xs">
          Adding it to your watchlist also requests it. You can turn this off in{" "}
          <ViewTransitionLink
            to="/settings/requests"
            className="hover:text-foreground underline underline-offset-2"
          >
            Settings › Requests
          </ViewTransitionLink>
          .
        </p>
      ) : null}
      {item.request.download ? (
        <RequestDownloadProgress download={item.request.download} className="mt-3 max-w-xs" />
      ) : null}
      {uhdState && UHDStateIcon ? (
        <p className="text-muted-foreground mt-2.5 flex items-center gap-1.5 text-xs">
          <UHDStateIcon aria-hidden="true" className="size-3.5" />
          4K: {uhdState === "pending" ? "Requested" : formatRequestDisplayState(uhdState)}
        </p>
      ) : null}
      {uhd?.download ? (
        <RequestDownloadProgress download={uhd.download} className="mt-2 max-w-xs" />
      ) : null}
      {confirmCancel ? (
        <CancelRequestDialog
          title={confirmCancel.is_4k ? `${item.title} (4K)` : item.title}
          open
          onOpenChange={(open) => {
            if (!open) setConfirmCancel(null);
          }}
          onConfirm={() => cancelRequest.mutate(confirmCancel.id)}
          isPending={cancelRequest.isPending}
        />
      ) : null}
      {seasonPicker ? (
        <RequestSeasonsDialog
          open={pickSeasons}
          onOpenChange={setPickSeasons}
          tmdbID={item.tmdb_id}
          title={item.title}
          detail={item}
        />
      ) : null}
    </>
  );
}

/**
 * The viewer's own regular or 4K request for this title, as `state` names,
 * while they can still cancel it.
 * The detail names the request when it is the account's own (or the viewer is
 * an admin), so that request is read directly and kept only if the account
 * made it; an account's active requests can run past one list page. A detail
 * without the ID falls back to the account's active requests. The title's
 * download progress comes from the detail, so another request downloading is
 * no reason to read the list again.
 */
function useOwnCancellableRequest(
  item: RequestMediaDetail,
  state: RequestState | undefined,
  is4K: boolean,
): MediaRequest | undefined {
  const { user } = useAuth();
  const mayCancel = state?.status === "pending" || state?.status === "approved";
  const requestID = state?.request_id;
  const named = useMediaRequest(requestID, { enabled: mayCancel });
  const mine = useMyMediaRequests(
    { outcome: "active" },
    { enabled: mayCancel && !requestID, pollDownloads: false },
  );
  if (!mayCancel) return undefined;
  if (requestID) {
    const request = named.data;
    return request && request.requested_by_user_id === user?.id && canCancelOwnRequest(request)
      ? request
      : undefined;
  }
  // The title's regular and 4K requests are separate; match the one asked for.
  return mine.data?.find(
    (request) =>
      request.media_type === item.media_type &&
      request.tmdb_id === item.tmdb_id &&
      Boolean(request.is_4k) === is4K &&
      canCancelOwnRequest(request),
  );
}
