package applications

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
)

// Answers maps a field key to its value: a string (text, date,
// single_choice), a float64 (number) or a []string (multiple_choice, and
// file: the file IDs).
type Answers map[string]any

// clone copies the answers (the slices too).
func (a Answers) clone() Answers {
	out := Answers{}
	for k, v := range a {
		if s, ok := v.([]string); ok {
			v = append([]string{}, s...)
		}
		out[k] = v
	}
	return out
}

// DecodeAnswers turns stored JSON into typed answers, using the form to
// know each field's type. Keys the form does not have are dropped.
func DecodeAnswers(form Form, raw []byte) (Answers, error) {
	out := Answers{}
	if len(raw) == 0 {
		return out, nil
	}
	var in map[string]json.RawMessage
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("decode answers: %w", err)
	}
	for k, v := range in {
		f, ok := form.field(k)
		if !ok {
			continue
		}
		val, msg := parseAnswer(f, v)
		if msg != "" {
			continue
		}
		if val != nil {
			out[k] = val
		}
	}
	return out, nil
}

// parseAnswer reads one JSON value for a field. It returns the value (nil
// for JSON null: remove) or a validation message.
func parseAnswer(f Field, raw json.RawMessage) (any, string) {
	if string(raw) == "null" {
		return nil, ""
	}
	switch f.Type {
	case TypeShortText, TypeLongText, TypeDate, TypeSingleChoice:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, "must be a string"
		}
		return s, ""
	case TypeNumber:
		var n float64
		if err := json.Unmarshal(raw, &n); err != nil {
			return nil, "must be a number"
		}
		return n, ""
	case TypeMultipleChoice, TypeFile:
		var list []string
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, "must be an array of strings"
		}
		if list == nil {
			list = []string{}
		}
		return list, ""
	}
	return nil, "is not a known field type"
}

// empty reports whether an answer counts as not given.
func empty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(x) == ""
	case []string:
		return len(x) == 0
	}
	return false
}

// checkValue validates a (non-empty) answer against its field's rules.
// fileIDs are the IDs of the files uploaded for that field. It returns the
// cleaned value or a message.
func checkValue(f Field, v any, fileIDs []string) (any, string) {
	switch f.Type {
	case TypeShortText, TypeLongText:
		s := strings.TrimSpace(v.(string))
		if f.Type == TypeShortText {
			s = strings.Join(strings.Fields(s), " ")
		}
		if n := runes(s); n > f.textMax() {
			return nil, fmt.Sprintf("must be at most %d characters", f.textMax())
		} else if f.MinLength != nil && n < *f.MinLength {
			return nil, fmt.Sprintf("must be at least %d characters", *f.MinLength)
		}
		return s, ""
	case TypeDate:
		s := strings.TrimSpace(v.(string))
		if !validDate(s) {
			return nil, "must be a date (YYYY-MM-DD)"
		}
		if f.MinDate != nil && s < *f.MinDate {
			return nil, "must be on or after " + *f.MinDate
		}
		if f.MaxDate != nil && s > *f.MaxDate {
			return nil, "must be on or before " + *f.MaxDate
		}
		return s, ""
	case TypeNumber:
		n := v.(float64)
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return nil, "must be a number"
		}
		if f.Min != nil && n < *f.Min {
			return nil, "must be at least " + formatNumber(*f.Min)
		}
		if f.Max != nil && n > *f.Max {
			return nil, "must be at most " + formatNumber(*f.Max)
		}
		return n, ""
	case TypeSingleChoice:
		s := v.(string)
		if !f.hasOption(s) {
			return nil, "must be one of the options"
		}
		return s, ""
	case TypeMultipleChoice:
		list := v.([]string)
		seen := map[string]bool{}
		out := []string{}
		for _, s := range list {
			if !f.hasOption(s) {
				return nil, fmt.Sprintf("%q is not one of the options", s)
			}
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		if f.Min != nil && float64(len(out)) < *f.Min {
			return nil, fmt.Sprintf("choose at least %s", formatNumber(*f.Min))
		}
		if f.Max != nil && float64(len(out)) > *f.Max {
			return nil, fmt.Sprintf("choose at most %s", formatNumber(*f.Max))
		}
		return out, ""
	case TypeFile:
		list := v.([]string)
		out := []string{}
		for _, id := range list {
			if !slices.Contains(fileIDs, id) {
				return nil, "must list files uploaded for this question"
			}
			if !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		if len(out) > f.maxFiles() {
			return nil, fmt.Sprintf("at most %d files", f.maxFiles())
		}
		return out, ""
	}
	return nil, "is not a known field type"
}

func (f Field) hasOption(v string) bool {
	return slices.ContainsFunc(f.Options, func(o Option) bool { return o.Value == v })
}

func formatNumber(n float64) string { return strconv.FormatFloat(n, 'f', -1, 64) }

// Visibility returns which fields are visible for the answers: a field
// with visible_if is visible when the field it depends on is visible and
// its answer is (single choice) or contains (multiple choice) one of the
// values. visible_if only looks back, so one pass in order is enough.
func Visibility(form Form, answers Answers) map[string]bool {
	out := map[string]bool{}
	for _, f := range form.Fields() {
		v := f.VisibleIf
		if v == nil {
			out[f.Key] = true
			continue
		}
		if !out[v.Field] {
			out[f.Key] = false
			continue
		}
		match := false
		switch a := answers[v.Field].(type) {
		case string:
			match = slices.Contains(v.values(), a)
		case []string:
			match = slices.ContainsFunc(a, func(s string) bool { return slices.Contains(v.values(), s) })
		}
		out[f.Key] = match
	}
	return out
}

// MissingRequired returns the keys of the required, visible fields without
// an answer, in form order.
func MissingRequired(form Form, answers Answers) []string {
	visible := Visibility(form, answers)
	out := []string{}
	for _, f := range form.Fields() {
		if f.Required && visible[f.Key] && empty(answers[f.Key]) {
			out = append(out, f.Key)
		}
	}
	return out
}

// mergeAnswers applies an autosave: each key is validated (but a missing
// required answer is allowed until submit); null removes an answer.
func mergeAnswers(form Form, current Answers, patch map[string]json.RawMessage, filesByField map[string][]string) (Answers, error) {
	errs := fieldErrors{}
	out := current.clone()
	for k, raw := range patch {
		path := "answers." + k
		f, ok := form.field(k)
		if !ok {
			errs.add(path, "is not a question of this form")
			continue
		}
		v, msg := parseAnswer(f, raw)
		if msg != "" {
			errs.add(path, msg)
			continue
		}
		if empty(v) {
			delete(out, k)
			continue
		}
		clean, msg := checkValue(f, v, filesByField[k])
		if msg != "" {
			errs.add(path, msg)
			continue
		}
		out[k] = clean
	}
	if err := errs.err(); err != nil {
		return nil, err
	}
	return out, nil
}

// finalAnswers is what a submission keeps: the answers of the visible
// fields only (a hidden field's answer is dropped), every one valid, and
// every required visible field answered.
func finalAnswers(form Form, answers Answers, filesByField map[string][]string) (Answers, error) {
	visible := Visibility(form, answers)
	errs := fieldErrors{}
	out := Answers{}
	for _, f := range form.Fields() {
		v, given := answers[f.Key]
		if !visible[f.Key] {
			continue
		}
		if !given || empty(v) {
			if f.Required {
				errs.add("answers."+f.Key, "is required")
			}
			continue
		}
		clean, msg := checkValue(f, v, filesByField[f.Key])
		if msg != "" {
			errs.add("answers."+f.Key, msg)
			continue
		}
		out[f.Key] = clean
	}
	if err := errs.err(); err != nil {
		return nil, err
	}
	return out, nil
}

// display writes an answer as text: the option labels for choices.
func display(f Field, v any) *string {
	if empty(v) {
		return nil
	}
	var s string
	switch x := v.(type) {
	case string:
		s = x
		if f.Type == TypeSingleChoice {
			s = f.optionLabel(x)
		}
	case float64:
		s = formatNumber(x)
	case []string:
		if f.Type == TypeFile {
			s = fmt.Sprintf("%d file(s)", len(x))
			break
		}
		labels := make([]string, 0, len(x))
		for _, o := range x {
			labels = append(labels, f.optionLabel(o))
		}
		s = strings.Join(labels, ", ")
	}
	return &s
}

func (f Field) optionLabel(v string) string {
	for _, o := range f.Options {
		if o.Value == v {
			return o.Label
		}
	}
	return v
}
