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
// a <span> with the active styling, and the other tab must be a link
// pointing at the current URL with the right query parameters swapped
// in/out. A regression that broke WithoutValues('sort', '*') (which
// panicked in an earlier revision) would be caught here.
func TestModelsSortControlRendersActiveTabs(t *testing.T) {
	current, _ := url.Parse("http://xolo.test/models?range=7d&show_all=true")

	cases := []struct {
		name        string
		currentSort string
		// activeLabel is the text of the tab rendered as a <span>.
		activeLabel string
		// inactiveLabel is the text of the tab rendered as an <a>.
		inactiveLabel string
		// hrefMustContain and hrefMustNotContain pin the URL mutations
		// applied to the current URL on the inactive tab.
		hrefMustContain    []string
		hrefMustNotContain []string
	}{
		{
			name:               "default sort highlights Usage and links to Price",
			currentSort:        "",
			activeLabel:        "Usage",
			inactiveLabel:      "Prix",
			hrefMustContain:    []string{"sort=price"},
			hrefMustNotContain: []string{"sort=price&sort=price"},
		},
		{
			name:               "price sort highlights Price and links back to Usage",
			currentSort:        "price",
			activeLabel:        "Prix",
			inactiveLabel:      "Usage",
			hrefMustContain:    []string{"range=7d", "show_all=true"},
			hrefMustNotContain: []string{"sort="},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := httpCtx.SetBaseURL(context.Background(), "http://xolo.test")
			ctx = httpCtx.SetCurrentURL(ctx, current)

			var out strings.Builder
			if err := modelsSortControl(tc.currentSort).Render(ctx, &out); err != nil {
				t.Fatalf("render: %v", err)
			}
			html := out.String()

			// The active tab must be a <span> with the active styling.
			activeSpan := `<span class="px-3 py-1 rounded bg-accent text-accent-foreground font-medium">` + tc.activeLabel + `</span>`
			if !strings.Contains(html, activeSpan) {
				t.Errorf("expected active span %q in:\n%s", activeSpan, html)
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

			// An active tab must never carry an href: there is nowhere
			// to click it. This guards against a future refactor that
			// renders the active state as an <a> and ends up emitting a
			// bogus href that could carry the wrong sort param.
			if strings.Contains(activeSpan, `href=`) {
				t.Errorf("active tab must not carry an href, got %q", activeSpan)
			}
		})
	}
}
