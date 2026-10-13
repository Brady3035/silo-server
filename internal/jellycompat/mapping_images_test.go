package jellycompat

import (
	"context"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
)

func TestItemImageTagsUseStableCanonicalSeed(t *testing.T) {
	m := newMapper(NewResourceIDCodec(), &config.Config{})
	updatedAt := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	item := upstreamListItem{
		ContentID:       "movie-1",
		Type:            "movie",
		Title:           "Movie",
		PosterURL:       "https://cdn.example.test/poster.jpg?sig=one",
		PosterPath:      "metadb://poster/movie-1",
		PosterThumbhash: "thumbhash",
		UpdatedAt:       updatedAt,
	}

	first := m.itemFromList(item, false, nil, nil)
	item.PosterURL = "https://cdn.example.test/poster.jpg?sig=two"
	second := m.itemFromList(item, false, nil, nil)

	if first.ImageTags["Primary"] == "" {
		t.Fatal("primary image tag is empty")
	}
	if first.ImageTags["Primary"] != second.ImageTags["Primary"] {
		t.Fatalf("image tag changed when only signed URL changed: %q vs %q", first.ImageTags["Primary"], second.ImageTags["Primary"])
	}

	item.UpdatedAt = updatedAt.Add(time.Second)
	third := m.itemFromList(item, false, nil, nil)
	if third.ImageTags["Primary"] == first.ImageTags["Primary"] {
		t.Fatalf("image tag did not change after UpdatedAt changed: %q", third.ImageTags["Primary"])
	}
}

func TestItemImageTagsFallbackToURLWhenCanonicalSeedMissing(t *testing.T) {
	m := newMapper(NewResourceIDCodec(), &config.Config{})
	item := upstreamListItem{
		ContentID: "movie-1",
		Type:      "movie",
		Title:     "Movie",
		PosterURL: "https://cdn.example.test/poster.jpg?sig=one",
	}

	first := m.itemFromList(item, false, nil, nil)
	item.PosterURL = "https://cdn.example.test/poster.jpg?sig=two"
	second := m.itemFromList(item, false, nil, nil)

	if first.ImageTags["Primary"] == second.ImageTags["Primary"] {
		t.Fatalf("fallback image tag did not change with URL: %q", first.ImageTags["Primary"])
	}
}

func TestItemImageTagsUseConfiguredSecret(t *testing.T) {
	item := upstreamListItem{
		ContentID:       "movie-1",
		Type:            "movie",
		Title:           "Movie",
		PosterURL:       "https://cdn.example.test/poster.jpg?sig=one",
		PosterPath:      "metadb://poster/movie-1",
		PosterThumbhash: "thumbhash",
		UpdatedAt:       time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC),
	}

	first := newMapper(NewResourceIDCodec(), &config.Config{
		Auth: config.AuthConfig{JWTSecret: "secret-one"},
	}).itemFromList(item, false, nil, nil)
	second := newMapper(NewResourceIDCodec(), &config.Config{
		Auth: config.AuthConfig{JWTSecret: "secret-two"},
	}).itemFromList(item, false, nil, nil)

	if first.ImageTags["Primary"] == second.ImageTags["Primary"] {
		t.Fatalf("signed image tag did not change with configured secret: %q", first.ImageTags["Primary"])
	}
}

func TestEpisodeListImageTagsUseStillThumbhash(t *testing.T) {
	secret := "image-secret"
	updatedAt := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	item := upstreamListItem{
		ContentID:       "episode-1",
		Type:            "episode",
		Title:           "Episode",
		PosterURL:       "https://cdn.example.test/still.jpg?sig=one",
		PosterPath:      "metadb://still/episode-1",
		PosterThumbhash: "poster-thumbhash",
		StillPath:       "metadb://still/episode-1",
		StillThumbhash:  "still-thumbhash",
		UpdatedAt:       updatedAt,
	}

	dto := newMapper(NewResourceIDCodec(), &config.Config{
		Auth: config.AuthConfig{JWTSecret: secret},
	}).itemFromList(item, false, nil, nil)
	expected := newImageTagSigner(secret).Tag(
		imageTagSeed(item.ContentID, "Primary", compatCardImageSize, item.StillPath, item.StillThumbhash, updatedAt),
		item.PosterURL,
	)

	if dto.ImageTags["Primary"] != expected {
		t.Fatalf("primary tag = %q, want still-thumbhash seed %q", dto.ImageTags["Primary"], expected)
	}
}

func TestLibraryImageTagsUseStablePosterPath(t *testing.T) {
	secret := "image-secret"
	codec := NewResourceIDCodec()
	library := upstreamUserLibrary{
		ID:         1,
		Name:       "Movies",
		Type:       "movies",
		PosterURL:  "https://cdn.example.test/library.jpg?sig=one",
		PosterPath: "library-posters/1/original.jpg",
	}

	first := newMapper(codec, &config.Config{
		Auth: config.AuthConfig{JWTSecret: secret},
	}).viewFromLibrary(library)
	library.PosterURL = "https://cdn.example.test/library.jpg?sig=two"
	second := newMapper(codec, &config.Config{
		Auth: config.AuthConfig{JWTSecret: secret},
	}).viewFromLibrary(library)

	routeID := codec.EncodeIntID(EncodedIDLibrary, int64(library.ID))
	expected := newImageTagSigner(secret).Tag(
		imageTagSeed(routeID, "Primary", compatCardImageSize, library.PosterPath, "", time.Time{}),
		library.PosterURL,
	)

	if first.ImageTags["Primary"] == "" {
		t.Fatal("library primary image tag is empty")
	}
	if first.ImageTags["Primary"] != second.ImageTags["Primary"] {
		t.Fatalf("library tag changed when only signed URL changed: %q vs %q", first.ImageTags["Primary"], second.ImageTags["Primary"])
	}
	if second.ImageTags["Primary"] != expected {
		t.Fatalf("library tag = %q, want %q", second.ImageTags["Primary"], expected)
	}
}

func TestApplySeriesImagesUsesCanonicalSeriesSeeds(t *testing.T) {
	secret := "image-secret"
	codec := NewResourceIDCodec()
	seriesContentID := "series-1"
	seriesRouteID := codec.EncodeStringID(EncodedIDItem, seriesContentID)
	updatedAt := time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC)
	dto := baseItemDTO{SeriesID: seriesRouteID}
	series := seriesImageSet{
		ContentID:         seriesContentID,
		PosterURL:         "https://cdn.example.test/poster.jpg?sig=one",
		PosterPath:        "metadb://poster/series-1",
		PosterThumbhash:   "poster-thumbhash",
		BackdropURL:       "https://cdn.example.test/backdrop.jpg?sig=one",
		BackdropPath:      "metadb://backdrop/series-1",
		BackdropThumbhash: "backdrop-thumbhash",
		UpdatedAt:         updatedAt,
	}

	newMapper(codec, &config.Config{
		Auth: config.AuthConfig{JWTSecret: secret},
	}).applySeriesImages(&dto, series)

	signer := newImageTagSigner(secret)
	expectedPrimary := signer.Tag(
		imageTagSeed(series.ContentID, "Primary", compatCardImageSize, series.PosterPath, series.PosterThumbhash, updatedAt),
		series.PosterURL,
	)
	expectedBackdrop := signer.Tag(
		imageTagSeed(series.ContentID, "Backdrop", compatCardImageSize, series.BackdropPath, series.BackdropThumbhash, updatedAt),
		series.BackdropURL,
	)

	if dto.SeriesPrimaryImageTag != expectedPrimary {
		t.Fatalf("SeriesPrimaryImageTag = %q, want %q", dto.SeriesPrimaryImageTag, expectedPrimary)
	}
	if len(dto.ParentBackdropImageTags) != 1 || dto.ParentBackdropImageTags[0] != expectedBackdrop {
		t.Fatalf("ParentBackdropImageTags = %#v, want [%q]", dto.ParentBackdropImageTags, expectedBackdrop)
	}
	if dto.ParentThumbImageTag != expectedBackdrop {
		t.Fatalf("ParentThumbImageTag = %q, want %q", dto.ParentThumbImageTag, expectedBackdrop)
	}
}

// An episode's show logo is found through ParentLogoItemId, as Jellyfin
// sets it; clients never look for a logo on the episode itself.
func TestApplySeriesImagesSetsParentLogo(t *testing.T) {
	codec := NewResourceIDCodec()
	secret := "logo-secret"
	updatedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seriesRouteID := codec.EncodeStringID(EncodedIDItem, "series:tvdb:75805")
	dto := baseItemDTO{SeriesID: seriesRouteID}
	series := seriesImageSet{
		ContentID: "series:tvdb:75805",
		LogoURL:   "/api/v2/artwork/tvdb/series/75805/logo/w500.abc.webp?exp=1&sig=x",
		LogoPath:  "tvdb/series/75805/logo.png",
		UpdatedAt: updatedAt,
	}

	newMapper(codec, &config.Config{
		Auth: config.AuthConfig{JWTSecret: secret},
	}).applySeriesImages(&dto, series)

	want := newImageTagSigner(secret).Tag(
		imageTagSeed(series.ContentID, "Logo", compatCardImageSize, series.LogoPath, "", updatedAt),
		series.LogoURL,
	)
	if dto.ParentLogoItemID != seriesRouteID {
		t.Fatalf("ParentLogoItemID = %q, want the series id %q", dto.ParentLogoItemID, seriesRouteID)
	}
	if dto.ParentLogoImageTag == "" || dto.ParentLogoImageTag != want {
		t.Fatalf("ParentLogoImageTag = %q, want %q", dto.ParentLogoImageTag, want)
	}

	// The tag names the same image as the series' own ImageTags["Logo"].
	seriesDTO := newMapper(codec, &config.Config{Auth: config.AuthConfig{JWTSecret: secret}}).itemFromList(upstreamListItem{
		ContentID: series.ContentID, Type: "series", Title: "Show",
		LogoURL: series.LogoURL, LogoPath: series.LogoPath, UpdatedAt: updatedAt,
	}, false, nil, nil)
	if seriesDTO.ImageTags["Logo"] != dto.ParentLogoImageTag {
		t.Fatalf("series Logo tag %q differs from the episode's ParentLogoImageTag %q", seriesDTO.ImageTags["Logo"], dto.ParentLogoImageTag)
	}

	// A series without a logo leaves the episode without one.
	bare := baseItemDTO{SeriesID: seriesRouteID}
	newMapper(codec, &config.Config{Auth: config.AuthConfig{JWTSecret: secret}}).applySeriesImages(&bare, seriesImageSet{ContentID: series.ContentID})
	if bare.ParentLogoItemID != "" || bare.ParentLogoImageTag != "" {
		t.Fatalf("ParentLogo set without a series logo: %q %q", bare.ParentLogoItemID, bare.ParentLogoImageTag)
	}
}

// List paths (Resume, Next Up, episode lists, collections) give episodes their
// series artwork through applyCompatEpisodeTarget; the logo comes with it and
// is remembered for the series image route.
func TestApplyCompatEpisodeTargetCarriesSeriesLogo(t *testing.T) {
	codec := NewResourceIDCodec()
	images := NewImageCache(time.Hour, time.Now)
	h := &ItemsHandler{codec: codec, mapper: newMapper(codec, &config.Config{}), images: images}
	seriesRouteID := codec.EncodeStringID(EncodedIDItem, "series-1")
	dto := baseItemDTO{ID: codec.EncodeStringID(EncodedIDItem, "ep-1"), SeriesID: seriesRouteID}
	target := compatEpisodeTarget{SeriesImages: seriesImageSet{
		ContentID: "series-1",
		LogoURL:   "https://cdn.example.test/series-1-logo.png?sig=one",
		LogoPath:  "metadb://logo/series-1",
	}}

	h.applyCompatEpisodeTarget(&dto, target)

	if dto.ParentLogoItemID != seriesRouteID || dto.ParentLogoImageTag == "" {
		t.Fatalf("ParentLogo = %q %q, want the series id and a tag", dto.ParentLogoItemID, dto.ParentLogoImageTag)
	}
	if got, ok := images.LookupSized(seriesRouteID, "Logo", "", compatCardImageSize); !ok || got != target.SeriesImages.LogoURL {
		t.Fatalf("remembered series logo = %q, %v; want %q", got, ok, target.SeriesImages.LogoURL)
	}
}

// ParentLogo follows the request's image controls like the other logo tags.
func TestItemsResponseOptionsStripParentLogo(t *testing.T) {
	withLogo := func() []baseItemDTO {
		return []baseItemDTO{{ParentLogoItemID: "series-1", ParentLogoImageTag: "logo-tag"}}
	}
	zero := 0
	cases := []struct {
		name  string
		query itemsQuery
		keep  bool
	}{
		{"no controls", itemsQuery{}, true},
		{"logo requested", itemsQuery{enableImageTypes: map[string]bool{"primary": true, "logo": true}}, true},
		{"logo not requested", itemsQuery{enableImageTypes: map[string]bool{"primary": true, "backdrop": true}}, false},
		{"images disabled", itemsQuery{disableImages: true}, false},
		{"image type limit zero", itemsQuery{imageTypeLimit: &zero}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			items := withLogo()
			applyItemsResponseOptions(items, tc.query)
			kept := items[0].ParentLogoItemID != "" && items[0].ParentLogoImageTag != ""
			cleared := items[0].ParentLogoItemID == "" && items[0].ParentLogoImageTag == ""
			if tc.keep && !kept || !tc.keep && !cleared {
				t.Fatalf("ParentLogo = %q %q, keep=%v", items[0].ParentLogoItemID, items[0].ParentLogoImageTag, tc.keep)
			}
		})
	}
}

// Episode detail builds the series image set from the series' catalog detail.
// Its ParentLogoImageTag must match the one lists give the same episode, and
// must not change when the logo's presigned URL is signed again.
func TestEpisodeDetailParentLogoMatchesListsAcrossPresigns(t *testing.T) {
	codec := NewResourceIDCodec()
	m := newMapper(codec, &config.Config{Auth: config.AuthConfig{JWTSecret: "logo-secret"}})
	updatedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seriesRouteID := codec.EncodeStringID(EncodedIDItem, "series-1")
	const logoPath = "local/series/series-1/aa/logo/original.r1.webp"

	detailTag := func(presignedURL string) string {
		// The production content service hands enrichEpisodeSeriesImages a
		// catalog detail converted by itemDetailToUpstream.
		series := itemDetailToUpstream(&catalog.ItemDetail{
			ContentID: "series-1", Type: "series", Title: "Show",
			LogoURL: presignedURL, LogoPath: logoPath, UpdatedAt: updatedAt,
		})
		h := &ItemsHandler{content: &countingContentService{seriesDetail: &series}, codec: codec, mapper: m, images: NewImageCache(time.Hour, time.Now)}
		dto := baseItemDTO{SeriesID: seriesRouteID}
		h.enrichEpisodeSeriesImages(context.Background(), &Session{}, &dto, "series-1", map[string]seriesImageSet{})
		return dto.ParentLogoImageTag
	}
	first := detailTag("https://cdn.example.test/logo.webp?exp=1&sig=a")
	resigned := detailTag("https://cdn.example.test/logo.webp?exp=2&sig=b")
	if first == "" || first != resigned {
		t.Fatalf("detail ParentLogoImageTag changed with the presigned URL: %q then %q", first, resigned)
	}

	listDTO := baseItemDTO{SeriesID: seriesRouteID}
	m.applySeriesImages(&listDTO, seriesImageSet{
		ContentID: "series-1", LogoURL: "https://cdn.example.test/logo.webp?exp=3&sig=c", LogoPath: logoPath, UpdatedAt: updatedAt,
	})
	if listDTO.ParentLogoImageTag != first {
		t.Fatalf("detail tag %q differs from the list tag %q for the same series logo", first, listDTO.ParentLogoImageTag)
	}
}
