package guard

import (
	"strings"
	"unicode"
)

const dynamic = "\x00"

type scanner struct {
	text     []rune
	pos      int
	commands [][]string
	problems []string
}

func (s *scanner) peek(offset int) rune {
	if s.pos+offset >= len(s.text) {
		return 0
	}
	return s.text[s.pos+offset]
}

func (s *scanner) find(value string, start int) int {
	index := strings.Index(string(s.text[start:]), value)
	if index < 0 {
		return -1
	}
	return start + len([]rune(string(s.text[start:])[:index]))
}

func (s *scanner) scan(closer rune) {
	var words []string
	word := ""
	hasWord, redirecting := false, false
	depth := 0
	endWord := func() {
		if !hasWord {
			return
		}
		if redirecting {
			if !harmlessRedirects[word] {
				s.problems = append(s.problems, "it writes to a file")
			}
			redirecting = false
		} else {
			words = append(words, word)
		}
		word, hasWord = "", false
	}
	endCommand := func() {
		endWord()
		if len(words) > 0 {
			s.commands = append(s.commands, words)
		}
		words = nil
	}
	appendWord := func(value string) { word += value; hasWord = true }
	for s.pos < len(s.text) {
		char := s.peek(0)
		if closer != 0 && char == closer && depth == 0 {
			s.pos++
			endCommand()
			return
		}
		switch char {
		case ' ', '\t':
			endWord()
			s.pos++
		case '\\':
			if s.peek(1) != '\n' {
				value := ""
				if s.pos+1 < len(s.text) {
					value = string(s.peek(1))
				}
				appendWord(value)
			}
			s.pos += 2
		case '\n', ';', '|':
			endCommand()
			s.pos++
		case '&':
			if s.peek(1) == '>' {
				endWord()
				if s.peek(2) == '>' {
					s.pos += 3
				} else {
					s.pos += 2
				}
				redirecting = true
			} else {
				endCommand()
				s.pos++
			}
		case '(', ')':
			if closer != 0 {
				if char == '(' {
					depth++
				} else {
					depth--
				}
			}
			endCommand()
			s.pos++
		case '>':
			if hasWord && allDigits(word) {
				word = ""
				hasWord = false
			}
			endWord()
			redirecting = s.startRedirect()
		case '<':
			endWord()
			if s.peek(1) == '<' && s.peek(2) != '<' {
				s.problems = append(s.problems, "it uses a here-document")
			}
			if s.peek(1) == '<' {
				s.pos += 3
			} else {
				s.pos++
			}
		case '#':
			if !hasWord {
				end := s.find("\n", s.pos)
				if end < 0 {
					s.pos = len(s.text)
				} else {
					s.pos = end
				}
			} else {
				appendWord(string(char))
				s.pos++
			}
		case '\'':
			end := s.find("'", s.pos+1)
			if end < 0 {
				s.problems = append(s.problems, "it has an unterminated quote")
				s.pos = len(s.text)
			} else {
				appendWord(string(s.text[s.pos+1 : end]))
				s.pos = end + 1
			}
		case '"':
			appendWord(s.doubleQuoted())
		case '$':
			appendWord(s.dollar())
		case '`':
			appendWord(s.backtick())
		default:
			appendWord(string(char))
			s.pos++
		}
	}
	if closer != 0 {
		s.problems = append(s.problems, "a command substitution never closes")
	}
	endCommand()
}

// Python str.isdigit also recognizes these non-decimal Unicode digits.
func allDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if !unicode.IsDigit(char) && !strings.ContainsRune("\u00b2\u00b3\u00b9\u1369\u136a\u136b\u136c\u136d\u136e\u136f\u1370\u1371\u19da\u2070\u2074\u2075\u2076\u2077\u2078\u2079\u2080\u2081\u2082\u2083\u2084\u2085\u2086\u2087\u2088\u2089\u2460\u2461\u2462\u2463\u2464\u2465\u2466\u2467\u2468\u2474\u2475\u2476\u2477\u2478\u2479\u247a\u247b\u247c\u2488\u2489\u248a\u248b\u248c\u248d\u248e\u248f\u2490\u24ea\u24f5\u24f6\u24f7\u24f8\u24f9\u24fa\u24fb\u24fc\u24fd\u24ff\u2776\u2777\u2778\u2779\u277a\u277b\u277c\u277d\u277e\u2780\u2781\u2782\u2783\u2784\u2785\u2786\u2787\u2788\u278a\u278b\u278c\u278d\u278e\u278f\u2790\u2791\u2792\U00010a40\U00010a41\U00010a42\U00010a43\U00010e60\U00010e61\U00010e62\U00010e63\U00010e64\U00010e65\U00010e66\U00010e67\U00010e68\U00011052\U00011053\U00011054\U00011055\U00011056\U00011057\U00011058\U00011059\U0001105a\U0001f100\U0001f101\U0001f102\U0001f103\U0001f104\U0001f105\U0001f106\U0001f107\U0001f108\U0001f109\U0001f10a", char) {
			return false
		}
	}
	return true
}

func (s *scanner) startRedirect() bool {
	s.pos++
	switch s.peek(0) {
	case '>', '|':
		s.pos++
	case '&':
		s.pos++
		target := s.peek(0)
		if allDigits(string(target)) || target == '-' {
			s.pos++
			return false
		}
	case '(':
		return false
	}
	return true
}

func (s *scanner) doubleQuoted() string {
	s.pos++
	value := ""
	for s.pos < len(s.text) {
		char := s.peek(0)
		switch char {
		case '"':
			s.pos++
			return value
		case '\\':
			following := s.peek(1)
			if following == '\n' {
			} else if following != 0 && strings.ContainsRune("$`\"\\", following) {
				value += string(following)
			} else {
				value += string(char)
				if s.pos+1 < len(s.text) {
					value += string(following)
				}
			}
			s.pos += 2
		case '$':
			value += s.dollar()
		case '`':
			value += s.backtick()
		default:
			value += string(char)
			s.pos++
		}
	}
	s.problems = append(s.problems, "it has an unterminated quote")
	return value
}

func (s *scanner) dollar() string {
	following := s.peek(1)
	switch following {
	case '(':
		if s.peek(2) == '(' {
			return s.arithmetic()
		}
		s.pos += 2
		s.scan(')')
		return dynamic
	case '{':
		end := s.find("}", s.pos)
		stop := end
		if stop < 0 {
			stop = len(s.text)
		}
		body := string(s.text[s.pos:stop])
		if end < 0 || strings.Contains(body, "$(") || strings.Contains(body, "`") {
			s.problems = append(s.problems, "it hides a command inside ${...}")
		}
		s.pos = stop
		if end >= 0 {
			s.pos++
		}
		return dynamic
	case '\'':
		end := s.find("'", s.pos+2)
		if end < 0 {
			s.pos = len(s.text)
		} else {
			s.pos = end + 1
		}
		return dynamic
	case '"':
		s.pos++
		return ""
	}
	if unicode.IsLetter(following) || unicode.IsNumber(following) || following == '_' {
		s.pos++
		for unicode.IsLetter(s.peek(0)) || unicode.IsNumber(s.peek(0)) || s.peek(0) == '_' {
			s.pos++
		}
		return dynamic
	}
	if following != 0 && strings.ContainsRune("@*#?$!-", following) {
		s.pos += 2
		return dynamic
	}
	s.pos++
	return "$"
}

func (s *scanner) arithmetic() string {
	end := s.find("))", s.pos)
	stop := end
	if stop < 0 {
		stop = len(s.text)
	}
	body := string(s.text[s.pos+3 : stop])
	if end < 0 || strings.Contains(body, "$(") || strings.Contains(body, "`") {
		s.problems = append(s.problems, "it hides a command inside $((...))")
	}
	s.pos = stop
	if end >= 0 {
		s.pos += 2
	}
	return dynamic
}

func (s *scanner) backtick() string { s.pos++; s.scan('`'); return dynamic }

func checkCommandLine(text string) string {
	s := scanner{text: []rune(text)}
	s.scan(0)
	if len(s.problems) > 0 {
		return s.problems[0]
	}
	for _, words := range s.commands {
		if reason := checkWords(words); reason != "" {
			return reason
		}
	}
	return ""
}
