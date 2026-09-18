package component

import (
	"context"
	"net/url"
	"strings"
	"testing"

	httpCtx "github.com/xolo-gateway/xolo/internal/http/context"
)

// TestModelsSortControlRendersActiveTabs pins the segmented control built
// from modelsSortControl: the tab matching currentSort must be rendered as
// a <span> with the active styling, the other tab must be a link pointing
// at the current URL with the right query parameters swapped in/out, and
// the order-toggle button must flip the active order without disturbing
// the sort criterion. A regression that broke WithoutValues('sort', '*')
// (which panicked in an earlier revision) would be caught here.
func TestModelsSortControlRendersActiveTabs(t *testing.T) {
	cases := []struct {
		name        string
		currentSort string
		currentOrder string
		// activeLabel is the text of the tab rendered as a <span>.
		activeLabel string
		// inactiveLabel is the text of the tab rendered as an <a>.
		inactiveLabel string
		// hrefMustContain and hrefMustNotContain pin the URL mutations
		// applied to the current URL on the inactive tab.
		hrefMustContain    []string
		hrefMustNotContain []string
		// toggleNextOrder is the order param the toggle must emit in its
		// href (the opposite of currentOrder).
		toggleNextOrder string
	}{
		{
			name:             "default sort highlights Usage and links to Price",
			currentSort:      "",
			currentOrder:     "desc",
			activeLabel:      "Usage",
			inactiveLabel:    "Prix",
			hrefMustContain:  []string{"sort=price", "order=desc"},
			hrefMustNotContain: []string{"sort=price&sort=price"},
			toggleNextOrder:  "asc",
		},
		{
			name:             "price sort highlights Price and links back to Usage",
			currentSort:      "price",
			currentOrder:     "asc",
			activeLabel:      "Prix",
			inactiveLabel:    "Usage",
			hrefMustContain:  []string{"range=7d", "show_all=true", "order=asc"},
			hrefMustNotContain: []string{"sort="},
			toggleNextOrder:  "desc",
		},
		{
			name:             "price sort desc preserves order on the inactive tab",
			currentSort:      "price",
			currentOrder:     "desc",
			activeLabel:      "Prix",
			inactiveLabel:    "Usage",
			hrefMustContain:  []string{"order=desc"},
			hrefMustNotContain: []string{"order=desc&order=desc"},
			toggleNextOrder:  "asc",
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

			// The active tab must be a <span> with the active styling.
			activeSpan := `<span class="px-3 py-1 rounded bg-accent text-accent-foreground font-medium">` + tc.activeLabel + `</span>`
			if !strings.Contains(html, activeSpan) {
				t.Errorf("expected active span %q in:\n%s", activeSpan, html)
			}

			// An active tab must never carry an href: there is nowhere
			// to click it. This guards against a future refactor that
			// renders the active state as an <a> and ends up emitting a
			// bogus href that could carry the wrong sort param.
			if strings.Contains(activeSpan, `href=`) {
				t.Errorf("active tab must not carry an href, got %q", activeSpan)
			}

			// The inactive tab must be a link pointing at the right URL.
			// Use "<a " (with the trailing space) rather than a bare "<a"
			// so future decorative anchors inside the segmented control
			// (tooltips, etc.) cannot silently satisfy the assertion.
			inactiveLink := strings.Contains(html, "<a ")
			if !inactiveLink {
				t.Fatalf("expected an <a> for the inactive tab, got:\n%s", html)
			}

			// Extract the href of the inactive tab via a simple search
			// for the href= attribute immediately preceding the
			// inactive label.
			idx := strings.Index(html, tc.inactiveLabel)
			if idx < 0 {
				t.Fatalf("could not find inactive label %q in rendered html:\n%s", tc.inactiveLabel, html)
			}
			before := html[:idx]
			hrefStart := strings.LastIndex(before, `href="`)
			if hrefStart < 0 {
				t.Fatalf("could not find href attribute before %q", tc.inactiveLabel)
			}
			hrefEnd := strings.Index(html[hrefStart+len(`href="`):], `"`)
			if hrefEnd < 0 {
				t.Fatalf("unterminated href attribute before %q", tc.inactiveLabel)
			}
			href := html[hrefStart+len(`href="`) : hrefStart+len(`href="`)+hrefEnd]

			decoded, err := url.QueryUnescape(href)
			if err != nil {
				t.Fatalf("href %q is not valid: %v", href, err)
			}

			for _, want := range tc.hrefMustContain {
				if !strings.Contains(decoded, want) {
					t.Errorf("inactive tab href %q should contain %q", decoded, want)
				}
			}
			for _, unwanted := range tc.hrefMustNotContain {
				if strings.Contains(decoded, unwanted) {
					t.Errorf("inactive tab href %q must NOT contain %q", decoded, unwanted)
				}
			}

			// The order-toggle button must be present as a distinct
			// <a> with an aria-label, and its href must flip the
			// current order without duplicating it. templ may render
			// href either before or after aria-label, so we locate the
			// enclosing <a ...> segment and read the href from it.
			idxToggle := strings.Index(html, `aria-label="`)
			if idxToggle < 0 {
				t.Fatalf("expected the order-toggle button to carry an aria-label, got:\n%s", html)
			}
			aStart := strings.LastIndex(html[:idxToggle], "<a ")
			if aStart < 0 {
				t.Fatalf("could not find <a ...> for the order-toggle button:\n%s", html)
			}
			aEnd := strings.Index(html[aStart:], ">")
			if aEnd < 0 {
				t.Fatalf("unterminated <a ...> for the order-toggle button:\n%s", html)
			}
			aEnd += aStart
			toggleSegment := html[aStart:aEnd]
			hrefStart = strings.Index(toggleSegment, `href="`)
			if hrefStart < 0 {
				t.Fatalf("could not find href on the order-toggle button segment %q", toggleSegment)
			}
			hrefEnd = strings.Index(toggleSegment[hrefStart+len(`href="`):], `"`)
			if hrefEnd < 0 {
				t.Fatalf("unterminated href on the order-toggle button segment %q", toggleSegment)
			}
			toggleHref := toggleSegment[hrefStart+len(`href="`) : hrefStart+len(`href="`)+hrefEnd]
			decodedToggle, err := url.QueryUnescape(toggleHref)
			if err != nil {
				t.Fatalf("toggle href %q is not valid: %v", toggleHref, err)
			}
			wantOrder := "order=" + tc.toggleNextOrder
			if !strings.Contains(decodedToggle, wantOrder) {
				t.Errorf("order-toggle href %q should contain %q", decodedToggle, wantOrder)
			}
			// The toggle must not leave a duplicate `order` param on a
			// URL that already carried one.
			if strings.Contains(decodedToggle, "order="+tc.toggleNextOrder+"&order=") {
				t.Errorf("order-toggle href %q must NOT duplicate the order param", decodedToggle)
			}
		})
	}
}