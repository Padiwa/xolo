package component

import (
	"context"
	"net/url"
	"strings"
	"testing"

	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
	common "github.com/xolo-gateway/xolo/internal/http/handler/webui/common/component"
)

// TestModelsSortControlRendersActiveTabs pins the three-segment sort
// control built from modelsSortControl: Usage, Prix ↓, Prix ↑. The
// segment matching the current (sort, order) pair is rendered as a
// <button> with the active styling, the other two are rendered as
// <a> links pointing at the current URL with the right query
// parameters applied. Every link is a plain anchor except the active
// segment, which stays a <button> so clicking it does nothing. The
// control uses common.SegmentedNav so it inherits the same visual
// style as the period selector (24 h / 7 j / 30 j / 12 m).
func TestModelsSortControlRendersActiveTabs(t *testing.T) {
	cases := []struct {
		name         string
		currentSort  string
		currentOrder string
		// activeLabel is the visible text of the active segment.
		activeLabel string
		// inactiveLabels are the visible texts of the two inactive
		// segments, paired with the URL mutations their hrefs must
		// carry (and must NOT carry).
		inactiveLabels []inactiveSegment
	}{
		{
			name:           "default usage view highlights Usage and links to Prix ↓ / Prix ↑",
			currentSort:    "",
			currentOrder:   "desc",
			activeLabel:    "Usage",
			inactiveLabels: []inactiveSegment{
				{label: "Prix ↓", mustContain: []string{"sort=price", "order=desc", "range=7d", "show_all=true"}},
				{label: "Prix ↑", mustContain: []string{"sort=price", "order=asc", "range=7d", "show_all=true"}},
			},
		},
		{
			name:           "price desc view highlights Prix ↓ and links to Usage / Prix ↑",
			currentSort:    "price",
			currentOrder:   "desc",
			activeLabel:    "Prix ↓",
			inactiveLabels: []inactiveSegment{
				{label: "Usage", mustNotContain: []string{"sort=", "order="}, mustContain: []string{"range=7d", "show_all=true"}},
				{label: "Prix ↑", mustContain: []string{"sort=price", "order=asc", "range=7d", "show_all=true"}},
			},
		},
		{
			name:           "price asc view highlights Prix ↑ and links to Usage / Prix ↓",
			currentSort:    "price",
			currentOrder:   "asc",
			activeLabel:    "Prix ↑",
			inactiveLabels: []inactiveSegment{
				{label: "Usage", mustNotContain: []string{"sort=", "order="}, mustContain: []string{"range=7d", "show_all=true"}},
				{label: "Prix ↓", mustContain: []string{"sort=price", "order=desc", "range=7d", "show_all=true"}},
			},
		},
		{
			name:           "usage asc view highlights Usage and links to Prix ↓ / Prix ↑ (with order=asc stripped)",
			currentSort:    "",
			currentOrder:   "asc",
			activeLabel:    "Usage",
			inactiveLabels: []inactiveSegment{
				{label: "Prix ↓", mustContain: []string{"sort=price", "order=desc", "range=7d", "show_all=true"}},
				{label: "Prix ↑", mustContain: []string{"sort=price", "order=asc", "range=7d", "show_all=true"}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current, _ := url.Parse("http://xolo.test/models?range=7d&show_all=true")
			ctx := httpCtx.SetBaseURL(context.Background(), "http://xolo.test")
			ctx = httpCtx.SetCurrentURL(ctx, current)

			var out strings.Builder
			if err := modelsSortControl(tc.currentSort, tc.currentOrder).Render(ctx, &out); err != nil {
				t.Fatalf("render: %v", err)
			}
			html := out.String()

			// The active segment must be rendered as a <button> with
			// the active styling (bg-primary-tint + text-primary, as
			// defined by common.SegmentedNav).
			activeButton := extractSegment(html, tc.activeLabel)
			if activeButton == "" {
				t.Fatalf("could not find segment %q in:\n%s", tc.activeLabel, html)
			}
			if !strings.HasPrefix(activeButton, "<button") {
				t.Errorf("active segment %q must be a <button>, got:\n%s", tc.activeLabel, activeButton)
			}
			if !strings.Contains(activeButton, "bg-primary-tint") {
				t.Errorf("active segment %q must use bg-primary-tint (matching the period selector), got:\n%s", tc.activeLabel, activeButton)
			}
			if strings.Contains(activeButton, "href=") {
				t.Errorf("active segment must NOT carry an href, got:\n%s", activeButton)
			}

			// Each inactive segment must be an <a> whose href carries
			// the expected mutations.
			for _, seg := range tc.inactiveLabels {
				tag := extractSegment(html, seg.label)
				if tag == "" {
					t.Fatalf("could not find segment %q in:\n%s", seg.label, html)
				}
				if !strings.HasPrefix(tag, "<a ") {
					t.Errorf("inactive segment %q must be an <a>, got:\n%s", seg.label, tag)
				}
				href := extractHref(tag)
				if href == "" {
					t.Fatalf("could not find href on inactive segment %q", seg.label)
				}
				decoded, err := url.QueryUnescape(href)
				if err != nil {
					t.Fatalf("href %q is not valid: %v", href, err)
				}
				for _, want := range seg.mustContain {
					if !strings.Contains(decoded, want) {
						t.Errorf("inactive segment %q href %q should contain %q", seg.label, decoded, want)
					}
				}
				for _, unwanted := range seg.mustNotContain {
					if strings.Contains(decoded, unwanted) {
						t.Errorf("inactive segment %q href %q must NOT contain %q", seg.label, decoded, unwanted)
					}
				}
			}
		})
	}
}

// inactiveSegment pins the URL expectations for one inactive segment of
// the sort SegmentedNav.
type inactiveSegment struct {
	label          string
	mustContain    []string
	mustNotContain []string
}

// extractSegment returns the HTML tag (<button ...>...</button> or <a ...>...</a>)
// that wraps the given visible label. The label is assumed to be the
// inner text of the tag (e.g. >Usage< or >Prix ↑<), so we locate it
// and walk backwards to the nearest <a or <button opener.
func extractSegment(html string, label string) string {
	idx := strings.Index(html, label)
	if idx < 0 {
		return ""
	}
	// Walk back from the label to the nearest opening tag, picking the
	// last one whose name is 'a' or 'button'.
	searchFrom := idx
	for {
		lt := strings.LastIndex(html[:searchFrom], "<")
		if lt < 0 {
			return ""
		}
		// Read the tag name (chars until whitespace or '>').
		rest := html[lt+1:]
		end := strings.IndexAny(rest, " >\n\t")
		if end <= 0 {
			return ""
		}
		name := rest[:end]
		if name == "a" || name == "button" {
			openEnd := strings.Index(html[lt:], ">")
			if openEnd < 0 {
				return ""
			}
			openEnd += lt
			closeTag := "</" + name + ">"
			closeIdx := strings.Index(html[openEnd:], closeTag)
			if closeIdx < 0 {
				return ""
			}
			closeIdx += openEnd
			return html[lt : closeIdx+len(closeTag)]
		}
		// Some other tag (e.g. <span>): keep walking back.
		searchFrom = lt
	}
}

// extractHref returns the href attribute value of an anchor tag.
func extractHref(tag string) string {
	start := strings.Index(tag, `href="`)
	if start < 0 {
		return ""
	}
	start += len(`href="`)
	end := strings.Index(tag[start:], `"`)
	if end < 0 {
		return ""
	}
	return tag[start : start+end]
}

// TestModelsPageRangeFormHidesOrderWhenNotExplicit pins the rule that the
// range form only round-trips the `order` hidden input when the request
// explicitly carried ?order=... . On the default usage view (no order
// param) the form must stay clean, so the user's bookmark of /models
// does not pick up a redundant ?order=desc suffix just from changing
// the period.
func TestModelsPageRangeFormHidesOrderWhenNotExplicit(t *testing.T) {
	current, _ := url.Parse("http://xolo.test/models")
	ctx := httpCtx.SetBaseURL(context.Background(), "http://xolo.test")
	ctx = httpCtx.SetCurrentURL(ctx, current)

	vmodel := ModelsPageVModel{
		AppLayoutVModel: common.AppLayoutVModel{
			Breadcrumbs: []common.BreadcrumbItem{{Label: "X", Href: ""}},
		},
		Range:         "7d",
		Order:         "desc",
		OrderExplicit: false, // default view: no ?order=... on the request
	}

	var out strings.Builder
	if err := ModelsPage(vmodel).Render(ctx, &out); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := out.String()

	if strings.Contains(html, `name="order"`) {
		t.Errorf("default view must NOT emit a hidden order input, got:\n%s", html)
	}
}

// TestModelsPageRangeFormRoundTripsExplicitOrder confirms the opposite:
// when the request explicitly carried ?order=desc, the range form must
// emit the hidden input so the order survives a period change.
func TestModelsPageRangeFormRoundTripsExplicitOrder(t *testing.T) {
	current, _ := url.Parse("http://xolo.test/models?order=desc")
	ctx := httpCtx.SetBaseURL(context.Background(), "http://xolo.test")
	ctx = httpCtx.SetCurrentURL(ctx, current)

	vmodel := ModelsPageVModel{
		AppLayoutVModel: common.AppLayoutVModel{
			Breadcrumbs: []common.BreadcrumbItem{{Label: "X", Href: ""}},
		},
		Range:         "7d",
		Order:         "desc",
		OrderExplicit: true,
	}

	var out strings.Builder
	if err := ModelsPage(vmodel).Render(ctx, &out); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := out.String()

	if !strings.Contains(html, `name="order" value="desc"`) {
		t.Errorf("explicit ?order=desc view must emit the hidden input, got:\n%s", html)
	}
}
