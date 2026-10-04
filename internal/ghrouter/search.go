package ghrouter

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"
)

func malformedSearch(message string) error {
	return newRequestError(http.StatusBadRequest, "malformed_search", errors.New(message))
}

func targetList(targets map[string]routeTarget) []routeTarget {
	result := make([]routeTarget, 0, len(targets))
	for _, target := range targets {
		result = append(result, target)
	}
	return result
}

// Search operators separate qualifiers; routing does not evaluate the expression.
func addSearchTargets(targets map[string]routeTarget, query string) error {
	for len(query) > 0 {
		query = strings.TrimLeftFunc(query, unicode.IsSpace)
		if query == "" {
			break
		}
		if query[0] == '(' || query[0] == ')' {
			query = query[1:]
			continue
		}
		end := 0
		quoted := false
		for end < len(query) {
			c := query[end]
			if quoted && c == '\\' {
				end += 2
				continue
			}
			if c == '"' {
				quoted = !quoted
			}
			r, size := utf8.DecodeRuneInString(query[end:])
			if !quoted && (c == '(' || c == ')' || unicode.IsSpace(r)) {
				break
			}
			end += size
		}
		unterminated := quoted || end > len(query)
		end = min(end, len(query))
		token := query[:end]
		query = query[end:]
		if token[0] == '"' || token[0] == '-' {
			continue
		}
		key, value, ok := strings.Cut(token, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(key)
		if key != "repo" && key != "org" && key != "user" {
			continue
		}
		if unterminated {
			return malformedSearch("unterminated search target")
		}
		if strings.HasPrefix(value, "\"") {
			var decoded string
			if err := json.Unmarshal([]byte(value), &decoded); err != nil {
				return malformedSearch("invalid quoted search target")
			}
			value = decoded
		}
		if key == "repo" {
			repository, ok := canonicalRepository(value)
			if !ok {
				return malformedSearch("invalid search repository")
			}
			owner, name, _ := strings.Cut(repository, "/")
			addRepositoryTarget(targets, owner, name)
		} else {
			owner, ok := canonicalName(value)
			if !ok {
				return malformedSearch("invalid search owner")
			}
			addOwnerTarget(targets, owner)
		}
		if len(targets) > maxTargets {
			return malformedSearch("too many routing targets")
		}
	}
	return nil
}

type graphQLToken struct {
	text        string
	stringValue bool
	blockString bool
}

// Tokenizing keeps search fields distinct from comments and string contents.
func graphQLTokens(query string) []graphQLToken {
	var tokens []graphQLToken
	for len(query) > 0 {
		query = strings.TrimLeft(query, " \t\r\n,\ufeff")
		if query == "" {
			break
		}
		if query[0] == '#' {
			end := strings.IndexAny(query, "\r\n")
			if end < 0 {
				break
			}
			query = query[end:]
			continue
		}
		if strings.HasPrefix(query, `"""`) {
			end := 3
			for end < len(query) && !strings.HasPrefix(query[end:], `"""`) {
				if strings.HasPrefix(query[end:], `\"""`) {
					end += 4
				} else {
					end++
				}
			}
			if end >= len(query) {
				return tokens
			}
			tokens = append(tokens, graphQLToken{blockString: true})
			query = query[end+3:]
			continue
		}
		if query[0] == '"' {
			end := 1
			for end < len(query) && query[end] != '"' {
				if query[end] == '\\' {
					end++
				}
				end++
			}
			if end >= len(query) {
				return tokens
			}
			var value string
			if err := json.Unmarshal([]byte(query[:end+1]), &value); err != nil {
				return tokens
			}
			tokens = append(tokens, graphQLToken{text: value, stringValue: true})
			query = query[end+1:]
			continue
		}
		end := 0
		for end < len(query) && graphQLNameByte(query[end]) {
			end++
		}
		if end == 0 {
			end = 1
		}
		tokens = append(tokens, graphQLToken{text: query[:end]})
		query = query[end:]
	}
	return tokens
}

func graphQLNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

func (t graphQLToken) is(text string) bool {
	return !t.stringValue && !t.blockString && t.text == text
}

func (t graphQLToken) isName() bool {
	return !t.stringValue && !t.blockString && t.text != "" &&
		(t.text[0] == '_' || t.text[0] >= 'a' && t.text[0] <= 'z' || t.text[0] >= 'A' && t.text[0] <= 'Z')
}

func addGraphQLSearchTargets(targets map[string]routeTarget, query string, variables map[string]json.RawMessage) error {
	tokens := graphQLTokens(query)
	braces, parentheses := 0, 0
	for i, token := range tokens {
		if token.is("{") {
			braces++
		} else if token.is("}") {
			braces--
		} else if token.is("(") {
			parentheses++
		} else if token.is(")") {
			parentheses--
		}
		if !token.is("search") || braces <= 0 || parentheses != 0 || i+1 >= len(tokens) || !tokens[i+1].is("(") {
			continue
		}
		if i > 0 && (tokens[i-1].is("@") || tokens[i-1].is("$")) {
			continue
		}
		if err := addGraphQLSearchArguments(targets, tokens[i+2:], variables); err != nil {
			return err
		}
	}
	return nil
}

func addGraphQLSearchArguments(targets map[string]routeTarget, tokens []graphQLToken, variables map[string]json.RawMessage) error {
	depth := 0
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if token.is(")") && depth == 0 {
			return nil
		}
		if token.is("(") || token.is("[") || token.is("{") {
			depth++
		} else if token.is(")") || token.is("]") || token.is("}") {
			depth--
		}
		if depth != 0 || !token.is("query") || i+2 >= len(tokens) || !tokens[i+1].is(":") {
			continue
		}
		value := tokens[i+2]
		var search string
		if value.stringValue {
			i += 2
			search = value.text
		} else if value.is("$") && i+3 < len(tokens) && tokens[i+3].isName() {
			i += 3
			variable, ok := jsonString(variables[tokens[i].text])
			if !ok {
				continue
			}
			search = variable
		} else {
			continue
		}
		if err := addSearchTargets(targets, search); err != nil {
			return err
		}
	}
	return nil
}
