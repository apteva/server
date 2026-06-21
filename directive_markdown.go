package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var directiveHeadingRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+?)\s*$`)

const structuredDirectiveSections = `# Role
You are %s.

# Goals
- 

# Operating Rules
- Prefer direct, useful action over commentary.
- Ask before irreversible or high-blast-radius actions.

# Inputs and Events
- Treat user messages, app events, and channel messages as work requests.

# Tools and Integrations
- Use available tools when they materially improve the result.
- Never expose credentials or secrets in messages, directives, or logs.

# Schedule
- Work reactively unless a subscription, schedule, or user request says otherwise.

# Escalation and Safety
- Pause and ask when the next action is ambiguous, destructive, or externally visible.

# Tone
- Be concise, specific, and clear.

# Learning
- Add stable lessons here when evaluations or operators identify recurring behavior.`

func defaultStructuredDirective(agentName string) string {
	name := strings.TrimSpace(agentName)
	if name == "" {
		name = "this agent"
	}
	return fmt.Sprintf(structuredDirectiveSections, name)
}

func hasMarkdownDirectiveHeadings(s string) bool {
	return directiveHeadingRe.MatchString(s)
}

func appendDirectiveLearning(current string, additions []string) string {
	var clean []string
	for _, add := range additions {
		add = strings.TrimSpace(add)
		if add != "" {
			clean = append(clean, add)
		}
	}
	if len(clean) == 0 {
		return current
	}
	if !hasMarkdownDirectiveHeadings(current) {
		out := current
		for _, add := range clean {
			if strings.TrimSpace(out) == "" {
				out = add
			} else {
				out = strings.TrimRight(out, "\n") + "\n\n" + add
			}
		}
		return out
	}
	var bullets []string
	for _, add := range clean {
		bullets = append(bullets, directiveLearningBullet(add))
	}
	return directiveAppendToSection(current, "Learning", strings.Join(bullets, "\n"))
}

func directiveLearningBullet(add string) string {
	add = strings.Join(strings.Fields(add), " ")
	trimmed := strings.TrimSpace(add)
	if strings.HasPrefix(trimmed, "- ") || strings.HasPrefix(trimmed, "* ") {
		return trimmed
	}
	return "- " + trimmed
}

type serverDirectiveEdit struct {
	Mode    string `json:"mode"`
	Section string `json:"section"`
	Match   string `json:"match"`
	Content string `json:"content"`
}

func applyServerDirectiveEdits(current string, args map[string]any) (string, bool, error) {
	var edits []serverDirectiveEdit
	if raw, ok := args["directive_edits"]; ok {
		parsed, err := parseServerDirectiveEdits(raw)
		if err != nil {
			return current, false, err
		}
		edits = append(edits, parsed...)
	}

	mode := directiveStringArg(args, "directive_edit_mode")
	section := directiveStringArg(args, "directive_section")
	match := directiveStringArg(args, "directive_match")
	content := directiveStringArg(args, "directive_content")
	if mode != "" || section != "" || match != "" || content != "" {
		edits = append(edits, serverDirectiveEdit{
			Mode:    mode,
			Section: section,
			Match:   match,
			Content: content,
		})
	}
	if len(edits) == 0 {
		return current, false, nil
	}

	out := current
	for _, edit := range edits {
		next, err := applyServerDirectiveEdit(out, edit)
		if err != nil {
			return current, false, err
		}
		out = next
	}
	return out, true, nil
}

func parseServerDirectiveEdits(raw any) ([]serverDirectiveEdit, error) {
	switch v := raw.(type) {
	case string:
		if strings.TrimSpace(v) == "" {
			return nil, nil
		}
		var edits []serverDirectiveEdit
		if err := json.Unmarshal([]byte(v), &edits); err != nil {
			return nil, fmt.Errorf("directive_edits must be a JSON array: %w", err)
		}
		return edits, nil
	case []any:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, fmt.Errorf("directive_edits encode: %w", err)
		}
		var edits []serverDirectiveEdit
		if err := json.Unmarshal(data, &edits); err != nil {
			return nil, fmt.Errorf("directive_edits must contain edit objects: %w", err)
		}
		return edits, nil
	default:
		return nil, fmt.Errorf("directive_edits must be a JSON array")
	}
}

func directiveStringArg(args map[string]any, key string) string {
	v, _ := args[key].(string)
	return strings.TrimSpace(v)
}

func applyServerDirectiveEdit(current string, edit serverDirectiveEdit) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(edit.Mode))
	if mode == "" {
		mode = "section_append"
	}
	section := strings.TrimSpace(edit.Section)
	content := strings.TrimSpace(edit.Content)
	match := strings.TrimSpace(edit.Match)

	switch mode {
	case "section_append":
		if section == "" {
			return current, fmt.Errorf("directive_section is required for section_append")
		}
		if content == "" {
			return current, fmt.Errorf("directive_content is required for section_append")
		}
		return directiveAppendToSection(current, section, content), nil
	case "section_replace":
		if section == "" {
			return current, fmt.Errorf("directive_section is required for section_replace")
		}
		return directiveReplaceSection(current, section, content), nil
	case "section_replace_line":
		if section == "" || match == "" {
			return current, fmt.Errorf("directive_section and directive_match are required for section_replace_line")
		}
		return directiveReplaceLine(current, section, match, content, false)
	case "section_remove_line":
		if section == "" || match == "" {
			return current, fmt.Errorf("directive_section and directive_match are required for section_remove_line")
		}
		return directiveReplaceLine(current, section, match, "", true)
	default:
		return current, fmt.Errorf("unsupported directive_edit_mode %q", edit.Mode)
	}
}

func directiveAppendToSection(current, section, content string) string {
	content = strings.TrimSpace(content)
	if content == "" {
		return current
	}
	lines, start, contentStart, end := directiveSectionBounds(current, section)
	if start < 0 {
		base := strings.TrimRight(current, "\n")
		if base != "" {
			base += "\n\n"
		}
		return base + "# " + section + "\n" + content
	}
	insertAt := end
	for insertAt > contentStart && strings.TrimSpace(lines[insertAt-1]) == "" {
		insertAt--
	}
	insert := []string{}
	if hasNonBlankLine(lines[contentStart:insertAt]) {
		insert = append(insert, "")
	}
	insert = append(insert, strings.Split(content, "\n")...)
	if end < len(lines) {
		insert = append(insert, "")
	}
	next := append([]string{}, lines[:insertAt]...)
	next = append(next, insert...)
	next = append(next, lines[end:]...)
	return strings.TrimRight(strings.Join(next, "\n"), "\n")
}

func directiveReplaceSection(current, section, content string) string {
	lines, start, contentStart, end := directiveSectionBounds(current, section)
	if start < 0 {
		base := strings.TrimRight(current, "\n")
		if base != "" {
			base += "\n\n"
		}
		if strings.TrimSpace(content) == "" {
			return base + "# " + section
		}
		return base + "# " + section + "\n" + strings.TrimSpace(content)
	}
	replacement := []string{}
	if strings.TrimSpace(content) != "" {
		replacement = strings.Split(strings.TrimSpace(content), "\n")
	}
	next := append([]string{}, lines[:contentStart]...)
	next = append(next, replacement...)
	next = append(next, lines[end:]...)
	return strings.TrimRight(strings.Join(next, "\n"), "\n")
}

func directiveReplaceLine(current, section, match, content string, remove bool) (string, error) {
	lines, _, contentStart, end := directiveSectionBounds(current, section)
	if contentStart < 0 {
		return current, fmt.Errorf("directive section %q not found", section)
	}
	for i := contentStart; i < end; i++ {
		if strings.Contains(lines[i], match) {
			if remove {
				next := append([]string{}, lines[:i]...)
				next = append(next, lines[i+1:]...)
				return strings.TrimRight(strings.Join(next, "\n"), "\n"), nil
			}
			lines[i] = content
			return strings.TrimRight(strings.Join(lines, "\n"), "\n"), nil
		}
	}
	return current, fmt.Errorf("directive line containing %q not found in section %q", match, section)
}

func directiveSectionBounds(current, section string) ([]string, int, int, int) {
	lines := strings.Split(strings.ReplaceAll(current, "\r\n", "\n"), "\n")
	target := strings.ToLower(strings.TrimSpace(section))
	start := -1
	for i, line := range lines {
		if name, ok := directiveHeadingName(line); ok && strings.ToLower(name) == target {
			start = i
			break
		}
	}
	if start < 0 {
		return lines, -1, -1, -1
	}
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if _, ok := directiveHeadingName(lines[i]); ok {
			end = i
			break
		}
	}
	return lines, start, start + 1, end
}

func directiveHeadingName(line string) (string, bool) {
	m := directiveHeadingRe.FindStringSubmatch(line)
	if len(m) != 2 {
		return "", false
	}
	return strings.TrimSpace(strings.TrimRight(m[1], "#")), true
}

func hasNonBlankLine(lines []string) bool {
	for _, line := range lines {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}
