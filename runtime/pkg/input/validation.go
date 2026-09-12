package input

import (
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ValidateFormResponse validates all submitted values against a resolved form.
// It returns every failure in declaration order, followed by unknown fields.
func ValidateFormResponse(request FormRequest, response FormResponse) []string {
	values := response.Values
	if values == nil {
		values = map[string]any{}
	}
	declared := make(map[string]bool, len(request.Fields))
	var failures []string
	for _, field := range request.Fields {
		declared[field.Name] = true
		value, exists := values[field.Name]
		if !exists || isEmptyFormValue(value) {
			if field.Required {
				failures = append(failures, fmt.Sprintf("%s is required", field.Name))
			}
			continue
		}
		failures = append(failures, validateFormValue(field, value)...)
	}

	var unknown []string
	for name := range values {
		if !declared[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		failures = append(failures, fmt.Sprintf("%s is not declared", name))
	}
	return failures
}

func validateFormValue(field FormField, value any) []string {
	if field.Multiple {
		items, ok := formList(value)
		if !ok {
			return []string{fmt.Sprintf("%s must be a list", field.Name)}
		}
		var failures []string
		for _, item := range items {
			failures = append(failures, validateSingleFormValue(field, item)...)
		}
		return failures
	}
	if _, ok := formList(value); ok {
		return []string{fmt.Sprintf("%s must be a single value", field.Name)}
	}
	return validateSingleFormValue(field, value)
}

func validateSingleFormValue(field FormField, value any) []string {
	var failures []string
	var stringValue string
	var numericValue float64
	var hasString, hasNumber bool

	switch field.Type {
	case "text", "multiline", "secret", "choice", "select", "autocomplete", "file", "image", "date", "datetime", "":
		stringValue, hasString = value.(string)
		if !hasString {
			failures = append(failures, fmt.Sprintf("%s must be text", field.Name))
		}
	case "number":
		numericValue, hasNumber = formNumber(value)
		if !hasNumber {
			failures = append(failures, fmt.Sprintf("%s must be a number", field.Name))
		}
	case "integer":
		numericValue, hasNumber = formNumber(value)
		if !hasNumber || math.Trunc(numericValue) != numericValue {
			failures = append(failures, fmt.Sprintf("%s must be an integer", field.Name))
			hasNumber = false
		}
	case "boolean":
		if _, ok := formBoolean(value); !ok {
			failures = append(failures, fmt.Sprintf("%s must be a boolean", field.Name))
		}
	default:
		failures = append(failures, fmt.Sprintf("%s has unsupported type %q", field.Name, field.Type))
	}

	if hasString {
		if field.Type == "date" {
			if _, err := time.Parse("2006-01-02", stringValue); err != nil {
				failures = append(failures, fmt.Sprintf("%s must be a date", field.Name))
			}
		} else if field.Type == "datetime" && !validFormDatetime(stringValue) {
			failures = append(failures, fmt.Sprintf("%s must be a datetime", field.Name))
		}
		if len(field.Options) > 0 && !formOptionAllowed(field.Options, stringValue) {
			message := "%s is not an allowed option"
			if field.Multiple {
				message = "%s contains an unallowed option"
			}
			failures = append(failures, fmt.Sprintf(message, field.Name))
		}
		failures = append(failures, validateFormString(field, stringValue)...)
	}
	if hasNumber {
		failures = append(failures, validateFormNumber(field, numericValue)...)
	}
	return failures
}

func validateFormString(field FormField, value string) []string {
	if field.Validation == nil {
		return nil
	}
	var failures []string
	if field.Validation.MinLength != nil && len(value) < *field.Validation.MinLength {
		failures = append(failures, fmt.Sprintf("%s length below %d", field.Name, *field.Validation.MinLength))
	}
	if field.Validation.MaxLength != nil && len(value) > *field.Validation.MaxLength {
		failures = append(failures, fmt.Sprintf("%s length above %d", field.Name, *field.Validation.MaxLength))
	}
	if field.Validation.Pattern != "" {
		pattern, err := regexp.Compile(field.Validation.Pattern)
		if err != nil || !pattern.MatchString(value) {
			failures = append(failures, fmt.Sprintf("%s does not match pattern", field.Name))
		}
	}
	switch field.Validation.Format {
	case "", "text":
	case "email":
		address, err := mail.ParseAddress(value)
		if err != nil || address.Address != value {
			failures = append(failures, fmt.Sprintf("%s must be an email address", field.Name))
		}
	case "url", "uri":
		parsed, err := url.ParseRequestURI(value)
		if err != nil || parsed.Scheme == "" {
			failures = append(failures, fmt.Sprintf("%s must be a URL", field.Name))
		}
	}
	return failures
}

func validateFormNumber(field FormField, value float64) []string {
	if field.Validation == nil {
		return nil
	}
	var failures []string
	if minimum, ok := formNumber(field.Validation.Min); ok && value < minimum {
		failures = append(failures, fmt.Sprintf("%s is below %v", field.Name, minimum))
	}
	if maximum, ok := formNumber(field.Validation.Max); ok && value > maximum {
		failures = append(failures, fmt.Sprintf("%s is above %v", field.Name, maximum))
	}
	if field.Validation.Step > 0 {
		base := 0.0
		if minimum, ok := formNumber(field.Validation.Min); ok {
			base = minimum
		}
		remainder := math.Mod(value-base, field.Validation.Step)
		if math.Abs(remainder) > 1e-9 && math.Abs(remainder-field.Validation.Step) > 1e-9 {
			failures = append(failures, fmt.Sprintf("%s does not match step %v", field.Name, field.Validation.Step))
		}
	}
	return failures
}

func formList(value any) ([]any, bool) {
	switch typed := value.(type) {
	case []any:
		return typed, true
	case []string:
		items := make([]any, len(typed))
		for index, item := range typed {
			items[index] = item
		}
		return items, true
	default:
		return nil, false
	}
}

func formNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int8:
		return float64(typed), true
	case int16:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint8:
		return float64(typed), true
	case uint16:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case float32:
		return float64(typed), true
	case float64:
		return typed, !math.IsNaN(typed) && !math.IsInf(typed, 0)
	case json.Number:
		number, err := typed.Float64()
		return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	case string:
		number, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return number, err == nil && !math.IsNaN(number) && !math.IsInf(number, 0)
	default:
		return 0, false
	}
}

func formBoolean(value any) (bool, bool) {
	if typed, ok := value.(bool); ok {
		return typed, true
	}
	if typed, ok := value.(string); ok {
		parsed, err := strconv.ParseBool(strings.TrimSpace(typed))
		return parsed, err == nil
	}
	return false, false
}

func formOptionAllowed(options []Option, value string) bool {
	for _, option := range options {
		if option.Value == value {
			return true
		}
	}
	return false
}

func validFormDatetime(value string) bool {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02T15:04:05"} {
		if _, err := time.Parse(layout, value); err == nil {
			return true
		}
	}
	return false
}

func isEmptyFormValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(typed) == ""
	case []string:
		return len(typed) == 0
	case []any:
		return len(typed) == 0
	default:
		return false
	}
}
