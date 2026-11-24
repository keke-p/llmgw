package admin

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ParseModelfile converts an Ollama Modelfile into /api/create fields (from,
// system, template, license, parameters, messages). Current Ollama versions no
// longer accept a "modelfile" field, so the gateway translates it.
//
// Supported instructions: FROM, PARAMETER, TEMPLATE, SYSTEM, LICENSE, MESSAGE.
// ADAPTER and FROM <local file> need blob uploads and are rejected.
func ParseModelfile(text string) (map[string]any, error) {
	out := map[string]any{}
	params := map[string]any{}
	var messages []map[string]string
	var licenses []string

	s := strings.ReplaceAll(text, "\r\n", "\n")
	lineNo := 0
	for len(s) > 0 {
		var line string
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			line, s = s[:i], s[i+1:]
		} else {
			line, s = s, ""
		}
		lineNo++
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmd, rest := line, ""
		if i := strings.IndexAny(line, " \t"); i >= 0 {
			cmd, rest = line[:i], strings.TrimSpace(line[i+1:])
		}

		// Value: """multi-line""", "quoted" or the rest of the line.
		value := func() (string, error) {
			if strings.HasPrefix(rest, `"""`) {
				body := rest[3:]
				if j := strings.Index(body, `"""`); j >= 0 {
					return body[:j], nil
				}
				var sb strings.Builder
				sb.WriteString(body)
				for {
					if s == "" {
						return "", fmt.Errorf("line %d: unterminated \"\"\"", lineNo)
					}
					var l string
					if i := strings.IndexByte(s, '\n'); i >= 0 {
						l, s = s[:i], s[i+1:]
					} else {
						l, s = s, ""
					}
					lineNo++
					sb.WriteByte('\n')
					if j := strings.Index(l, `"""`); j >= 0 {
						sb.WriteString(l[:j])
						return strings.TrimPrefix(sb.String(), "\n"), nil
					}
					sb.WriteString(l)
				}
			}
			if len(rest) >= 2 && rest[0] == '"' && rest[len(rest)-1] == '"' {
				if u, err := strconv.Unquote(rest); err == nil {
					return u, nil
				}
				return rest[1 : len(rest)-1], nil
			}
			return rest, nil
		}

		switch strings.ToUpper(cmd) {
		case "FROM":
			v, err := value()
			if err != nil {
				return nil, err
			}
			if v == "" {
				return nil, fmt.Errorf("line %d: FROM needs a model name", lineNo)
			}
			if strings.HasPrefix(v, "/") || strings.HasPrefix(v, ".") || strings.HasPrefix(v, "~") ||
				strings.Contains(v, `\`) || strings.HasSuffix(strings.ToLower(v), ".gguf") {
				return nil, fmt.Errorf("line %d: FROM a local file is not supported; use a model name", lineNo)
			}
			out["from"] = v
		case "PARAMETER":
			key, val, _ := strings.Cut(strings.ReplaceAll(rest, "\t", " "), " ")
			key, val = strings.TrimSpace(key), strings.TrimSpace(val)
			if key == "" || val == "" {
				return nil, fmt.Errorf("line %d: PARAMETER needs a name and a value", lineNo)
			}
			rest = val
			v, err := value()
			if err != nil {
				return nil, err
			}
			key = strings.ToLower(key)
			if key == "stop" {
				prev, _ := params["stop"].([]string)
				params["stop"] = append(prev, v)
			} else {
				params[key] = paramValue(v)
			}
		case "TEMPLATE", "SYSTEM":
			v, err := value()
			if err != nil {
				return nil, err
			}
			out[strings.ToLower(cmd)] = v
		case "LICENSE":
			v, err := value()
			if err != nil {
				return nil, err
			}
			licenses = append(licenses, v)
		case "MESSAGE":
			role, content, ok := strings.Cut(rest, " ")
			role = strings.ToLower(strings.TrimSpace(role))
			if !ok || (role != "system" && role != "user" && role != "assistant") {
				return nil, fmt.Errorf("line %d: MESSAGE needs a role (system/user/assistant) and content", lineNo)
			}
			rest = strings.TrimSpace(content)
			v, err := value()
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]string{"role": role, "content": v})
		case "ADAPTER":
			return nil, fmt.Errorf("line %d: ADAPTER is not supported", lineNo)
		default:
			return nil, fmt.Errorf("line %d: unknown instruction %q", lineNo, cmd)
		}
	}
	if _, ok := out["from"]; !ok {
		return nil, errors.New("modelfile: FROM is required")
	}
	if len(params) > 0 {
		out["parameters"] = params
	}
	if len(messages) > 0 {
		out["messages"] = messages
	}
	switch len(licenses) {
	case 0:
	case 1:
		out["license"] = licenses[0]
	default:
		out["license"] = licenses
	}
	return out, nil
}

// paramValue types a PARAMETER value: integer, float, bool or string.
func paramValue(v string) any {
	if i, err := strconv.ParseInt(v, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	if b, err := strconv.ParseBool(v); err == nil && (v == "true" || v == "false") {
		return b
	}
	return v
}
