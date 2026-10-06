package postgres

import "strings"

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likePattern turns a UI search into a LIKE pattern for ESCAPE '\': '*' is the only
// wildcard, and '%', '_' and '\' typed by a user match themselves.
func likePattern(search string) string {
	return strings.ReplaceAll(likeEscaper.Replace(search), "*", "%")
}
