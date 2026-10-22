package search

import (
	"strings"
	"unicode/utf8"

	"github.com/grbit/meme-search-telegram-bot/memesearch"
)

// overrides are the filters typed into one query; they replace the saved
// value of their dimension for that search only.
type overrides struct {
	query      string
	tags       []string // from #tag
	folders    []string // from dir:name
	allFolders bool     // dir:* drops the saved folder scope
}

const dirPrefix = "dir:"

// parseOverrides splits text into filters and the query. Only whole words
// count: "c#" stays text, a lone "#" or "dir:" too.
func parseOverrides(text string) overrides {
	var o overrides
	var words []string
	for _, w := range strings.Fields(text) {
		switch {
		case len(w) > 1 && w[0] == '#':
			if t := strings.TrimRight(w[1:], ",;"); t != "" {
				o.tags = appendNew(o.tags, t)
				continue
			}
		case len(w) > len(dirPrefix) && strings.EqualFold(w[:len(dirPrefix)], dirPrefix):
			if name := w[len(dirPrefix):]; name == "*" {
				o.allFolders = true
			} else {
				o.folders = appendNew(o.folders, name)
			}
			continue
		}
		words = append(words, w)
	}
	o.query = strings.Join(words, " ")
	return o
}

func appendNew(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// tagProblemOf checks tags against the API limits.
func tagProblemOf(tags []string) filterProblem {
	if len(tags) > memesearch.MaxTags {
		return tagsTooMany
	}
	for _, t := range tags {
		if utf8.RuneCountInString(t) > memesearch.MaxTagLen {
			return tagsTooLong
		}
	}
	return filtersOK
}

// foldersOK checks typed folders against the API limits: more would fail
// every search of that scope in the client's local check.
func foldersOK(folders []string) bool {
	if len(folders) > memesearch.MaxFolders {
		return false
	}
	for _, f := range folders {
		if utf8.RuneCountInString(f) > memesearch.MaxFolderLen {
			return false
		}
	}
	return true
}

// limitProblem checks the typed filters against the API limits, tags before
// folders. It is the one rule for every surface: the handlers refuse with it,
// Chargeable and config validation use it, so they cannot drift apart.
func (o overrides) limitProblem() filterProblem {
	if p := tagProblemOf(o.tags); p != filtersOK {
		return p
	}
	if !foldersOK(o.folders) {
		return foldersTooMany
	}
	return filtersOK
}

// limitMessage is the refusal for a limitProblem other than filtersOK, as a
// message id and its data.
func (o overrides) limitMessage(p filterProblem) (string, map[string]any) {
	switch p {
	case tagsTooMany:
		return "search.query_tags_too_many", map[string]any{"Max": memesearch.MaxTags}
	case tagsTooLong:
		return "search.query_tag_too_long", map[string]any{"Tag": longestTag(o.tags), "Len": memesearch.MaxTagLen}
	}
	return "search.query_folders_too_many", map[string]any{"Max": memesearch.MaxFolders, "Len": memesearch.MaxFolderLen}
}

// searchable returns the query and whether these overrides run a search:
// a usable query and filters within the limits. Unknown folder names are not
// judged here; that needs the server's folder list.
func (o overrides) searchable() (string, bool) {
	q, ok := searchQuery(o.query)
	return q, ok && o.limitProblem() == filtersOK
}
