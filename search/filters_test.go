package search

import (
	"strconv"
	"strings"
	"testing"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

func manyFilters(prefix string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = prefix + "f" + strconv.Itoa(i)
	}
	return strings.Join(parts, " ")
}

// filterInputs cover every reason a typed search is refused before the API.
var filterInputs = []struct {
	name string
	text string
}{
	{"blank", "   "},
	{"filters only", "#cat dir:x"},
	{"long query", strings.Repeat("a", memesearch.MaxQueryLen+1)},
	{"too many tags", "cat " + manyFilters("#", memesearch.MaxTags+1)},
	{"tag too long", "cat #" + strings.Repeat("t", memesearch.MaxTagLen+1)},
	{"too many folders", "cat " + manyFilters("dir:", memesearch.MaxFolders+1)},
	{"folder too long", "cat dir:" + strings.Repeat("d", memesearch.MaxFolderLen+1)},
	{"tags and folders over", "cat " + manyFilters("#", memesearch.MaxTags+1) + " " + manyFilters("dir:", memesearch.MaxFolders+1)},
	{"former group code", "g:abcdef"}, // plain query text since group scopes were dropped
	{"valid", "cat"},
	{"valid with filters", "cat #a dir:x dir:*"},
}

// Chargeable runs before the handler (in the access middleware), so it must
// say "charge" exactly when the handler goes on to query the API: otherwise a
// refused search costs the user a search, or a real one costs nothing.
// Folders pass through unchecked here (no folder list), so only the typed
// limits decide.
func TestChargeableAgreesWithTheHandlers(t *testing.T) {
	for _, tc := range filterInputs {
		t.Run("text/"+tc.name, func(t *testing.T) {
			api := &fakeAPI{results: memeRange(1)}
			e := newEnv(t, api, &fakeMedia{uploads: true})
			u := textUpdate(tc.text)
			e.process(u)
			if got, reached := Chargeable(u), api.searchCount() > 0; got != reached {
				t.Fatalf("Chargeable = %v, handler searched = %v", got, reached)
			}
		})
		t.Run("inline/"+tc.name, func(t *testing.T) {
			api := &fakeAPI{results: memeRange(1)}
			e := newEnv(t, api, &fakeMedia{uploads: true})
			u := inlineUpdate(tc.text)
			e.process(u)
			if got, reached := Chargeable(u), api.searchCount() > 0; got != reached {
				t.Fatalf("Chargeable = %v, handler searched = %v", got, reached)
			}
		})
	}
}
