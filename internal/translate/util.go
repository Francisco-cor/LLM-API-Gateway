package translate

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

func clamp(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func normalizeRole(role string) string {
	switch role {
	case "developer":
		return "system"
	case "function":
		return "tool"
	default:
		return role
	}
}

func parseStopSequences(stop any) ([]string, error) {
	switch value := stop.(type) {
	case nil:
		return nil, nil
	case string:
		if value == "" {
			return nil, nil
		}
		return []string{value}, nil
	case []string:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if item != "" {
				out = append(out, item)
			}
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(value))
		for _, item := range value {
			text, ok := item.(string)
			if !ok {
				return nil, invalidRequest("stop must be a string or an array of strings")
			}
			if text != "" {
				out = append(out, text)
			}
		}
		return out, nil
	default:
		return nil, invalidRequest("stop must be a string or an array of strings")
	}
}

func joinSystem(parts []string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

func parseDataURI(raw string) (mediaType, data string, ok bool) {
	rest := strings.TrimPrefix(raw, "data:")
	comma := strings.IndexByte(rest, ',')
	if comma < 0 {
		return "", "", false
	}
	meta := rest[:comma]
	if !strings.HasSuffix(meta, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(meta, ";base64"), rest[comma+1:], true
}

func isHTTPURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

func responseCreated() int64 {
	return time.Now().Unix()
}

func jsonObject(text string) (map[string]any, bool) {
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text), &decoded); err != nil || decoded == nil {
		return nil, false
	}
	return decoded, true
}
