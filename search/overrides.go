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
func tagProblemOf(tags []string) tagProblem {
	if len(tags) > memesearch.MaxTags {
		return tagsTooMany
	}
	for _, t := range tags {
		if utf8.RuneCountInString(t) > memesearch.MaxTagLen {
			return tagsTooLong
		}
	}
	return tagsOK
}
