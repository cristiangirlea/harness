package pluginproto

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	maxProtocols    = 16
	maxCapabilities = 64
	maxEvidence     = 64
	maxDescription  = 256
)

var (
	programName    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	programVersion = regexp.MustCompile(`^[A-Za-z0-9._+-]{1,64}$`)
	protocolName   = regexp.MustCompile(`^[a-z0-9.-]{1,16}$`)
	callID         = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	evidenceHash   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Negotiate returns the first offered version that supported contains.
func Negotiate(offered, supported []string) (string, bool) {
	for _, version := range offered {
		if slices.Contains(supported, version) {
			return version, true
		}
	}
	return "", false
}

// ParseHostLine decodes one line a host wrote: *Hello, *Call or *Cancel.
func ParseHostLine(line []byte) (any, error) {
	fields, kind, err := parseLine(line)
	if err != nil {
		return nil, err
	}
	var frame any
	switch kind {
	case TypeHello:
		frame = new(Hello)
	case TypeCall:
		frame = new(Call)
	case TypeCancel:
		frame = new(Cancel)
	default:
		return nil, fmt.Errorf("type: unknown host frame %q", kind)
	}
	if err := decodeFields(fields, kind, reflect.ValueOf(frame).Elem()); err != nil {
		return nil, err
	}
	switch frame := frame.(type) {
	case *Hello:
		if err := validateHello(frame, fields["limits"]); err != nil {
			return nil, err
		}
	case *Call:
		if err := validateCall(frame, fields["timeout_ms"] != nil); err != nil {
			return nil, err
		}
	case *Cancel:
		if err := validateID("cancel.id", frame.ID); err != nil {
			return nil, err
		}
	}
	return frame, nil
}

// ParsePluginLine decodes one line a plugin wrote: *Ready, *Event, *Result or *Error.
func ParsePluginLine(line []byte) (any, error) {
	fields, kind, err := parseLine(line)
	if err != nil {
		return nil, err
	}
	var frame any
	switch kind {
	case TypeReady:
		frame = new(Ready)
	case TypeEvent:
		frame = new(Event)
	case TypeResult:
		frame = new(Result)
	case TypeError:
		frame = new(Error)
	default:
		return nil, fmt.Errorf("type: unknown plugin frame %q", kind)
	}
	if err := decodeFields(fields, kind, reflect.ValueOf(frame).Elem()); err != nil {
		return nil, err
	}
	switch frame := frame.(type) {
	case *Ready:
		err = validateReady(frame)
	case *Event:
		err = validateEvent(frame, fields["text"] != nil)
	case *Result:
		err = validateResult(frame)
	case *Error:
		err = validateError(frame, fields["id"] != nil)
	}
	if err != nil {
		return nil, err
	}
	return frame, nil
}

type objectFields map[string]json.RawMessage

func parseLine(line []byte) (objectFields, string, error) {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
	}
	if len(line) > MaxLine {
		return nil, "", fmt.Errorf("line: exceeds %d bytes", MaxLine)
	}
	if !utf8.Valid(line) {
		return nil, "", fmt.Errorf("line: invalid UTF-8")
	}
	if bytes.IndexByte(line, '\n') >= 0 {
		return nil, "", fmt.Errorf("line: contains a newline")
	}
	line = bytes.TrimLeft(line, " \t\r")
	if len(line) == 0 || line[0] != '{' {
		return nil, "", fmt.Errorf("line: expected a JSON object")
	}
	var fields objectFields
	if err := json.Unmarshal(line, &fields); err != nil {
		return nil, "", fmt.Errorf("line: %w", err)
	}
	var kind string
	raw, ok := fields["type"]
	if !ok {
		return nil, "", fmt.Errorf("type: required")
	}
	if err := json.Unmarshal(raw, &kind); err != nil {
		return nil, "", fmt.Errorf("type: %w", err)
	}
	if kind == "" {
		return nil, "", fmt.Errorf("type: expected a non-empty string")
	}
	return fields, kind, nil
}

// decodeFields uses exact JSON tag names because encoding/json matches struct fields case-insensitively.
func decodeFields(fields objectFields, path string, dst reflect.Value) error {
	for i := range dst.NumField() {
		name, _, _ := strings.Cut(dst.Type().Field(i).Tag.Get("json"), ",")
		if raw, ok := fields[name]; ok {
			if err := decodeValue(raw, path+"."+name, dst.Field(i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeValue(raw json.RawMessage, path string, dst reflect.Value) error {
	if payload, ok := dst.Addr().Interface().(*json.RawMessage); ok {
		switch raw[0] {
		case '{':
			*payload = raw
			return nil
		case 'n':
			return nil
		default:
			return fmt.Errorf("%s: expected a JSON object", path)
		}
	}
	if bytes.Equal(raw, []byte("null")) {
		return fmt.Errorf("%s: must not be null", path)
	}
	switch dst.Kind() {
	case reflect.Pointer:
		dst.Set(reflect.New(dst.Type().Elem()))
		return decodeValue(raw, path, dst.Elem())
	case reflect.Struct:
		var fields objectFields
		if err := json.Unmarshal(raw, &fields); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return decodeFields(fields, path, dst)
	case reflect.Slice:
		return decodeList(raw, path, dst)
	default:
		if err := json.Unmarshal(raw, dst.Addr().Interface()); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		return nil
	}
}

func decodeList(raw json.RawMessage, path string, dst reflect.Value) error {
	if raw[0] != '[' {
		return fmt.Errorf("%s: expected an array", path)
	}
	maxEntries := maxProtocols
	switch dst.Type().Elem() {
	case reflect.TypeFor[Capability]():
		maxEntries = maxCapabilities
	case reflect.TypeFor[Evidence]():
		maxEntries = maxEvidence
	}
	list := reflect.MakeSlice(dst.Type(), 0, 0)
	// The enclosing JSON was already validated, so only array entries need decoding here.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if _, err := decoder.Token(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for decoder.More() {
		index := list.Len()
		if index == maxEntries {
			return fmt.Errorf("%s: exceeds %d entries", path, maxEntries)
		}
		var item json.RawMessage
		if err := decoder.Decode(&item); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		value := reflect.New(dst.Type().Elem()).Elem()
		if err := decodeValue(item, fmt.Sprintf("%s[%d]", path, index), value); err != nil {
			return err
		}
		list = reflect.Append(list, value)
	}
	dst.Set(list)
	return nil
}

func validateHello(frame *Hello, limits json.RawMessage) error {
	if err := validateProtocols("hello.protocols", frame.Protocols); err != nil {
		return err
	}
	if err := validateProgram("hello.host", frame.Host); err != nil {
		return err
	}
	if frame.Limits != nil && frame.Limits.MaxLine <= 0 {
		var fields objectFields
		if err := json.Unmarshal(limits, &fields); err != nil {
			return fmt.Errorf("hello.limits: %w", err)
		}
		if _, present := fields["max_line"]; present {
			return fmt.Errorf("hello.limits.max_line: must be positive")
		}
	}
	return nil
}

func validateCall(frame *Call, hasTimeout bool) error {
	if err := validateID("call.id", frame.ID); err != nil {
		return err
	}
	if !programName.MatchString(frame.Capability) {
		return fmt.Errorf("call.capability: invalid name")
	}
	if hasTimeout && frame.TimeoutMS <= 0 {
		return fmt.Errorf("call.timeout_ms: must be positive")
	}
	return nil
}

func validateEnvelope(path string, version int, seq int64) error {
	if version != 1 {
		return fmt.Errorf("%s.v: must be 1", path)
	}
	if seq < 1 {
		return fmt.Errorf("%s.seq: must be positive", path)
	}
	return nil
}

func validateReady(frame *Ready) error {
	if err := validateEnvelope("ready", frame.V, frame.Seq); err != nil {
		return err
	}
	if !protocolName.MatchString(frame.Protocol) {
		return fmt.Errorf("ready.protocol: invalid protocol")
	}
	if err := validateProgram("ready.plugin", frame.Plugin); err != nil {
		return err
	}
	if len(frame.Capabilities) == 0 {
		return fmt.Errorf("ready.capabilities: at least one entry is required")
	}
	names := make(map[string]bool, len(frame.Capabilities))
	for i, capability := range frame.Capabilities {
		if !programName.MatchString(capability.Name) {
			return fmt.Errorf("ready.capabilities[%d].name: invalid name", i)
		}
		if names[capability.Name] {
			return fmt.Errorf("ready.capabilities: duplicate name %q", capability.Name)
		}
		names[capability.Name] = true
		if len(capability.Description) > maxDescription {
			return fmt.Errorf("ready.capabilities[%d].description: exceeds %d bytes", i, maxDescription)
		}
	}
	return nil
}

func validateEvent(frame *Event, hasText bool) error {
	if err := validateEnvelope("event", frame.V, frame.Seq); err != nil {
		return err
	}
	if err := validateID("event.id", frame.ID); err != nil {
		return err
	}
	if frame.Kind != KindInfo && frame.Kind != KindProgress {
		return fmt.Errorf("event.kind: unknown kind %q", frame.Kind)
	}
	if !hasText {
		return fmt.Errorf("event.text: required")
	}
	if len(frame.Text) > MaxEventText {
		return fmt.Errorf("event.text: exceeds %d bytes", MaxEventText)
	}
	return nil
}

func validateResult(frame *Result) error {
	if err := validateEnvelope("result", frame.V, frame.Seq); err != nil {
		return err
	}
	if err := validateID("result.id", frame.ID); err != nil {
		return err
	}
	switch frame.Status {
	case StatusFailed:
		if frame.Error == nil {
			return fmt.Errorf("result.error: required for failed status")
		}
		if err := validateProblem("result.error", frame.Error.Code, frame.Error.Message); err != nil {
			return err
		}
	case StatusOK, StatusCanceled:
		if frame.Error != nil {
			return fmt.Errorf("result.error: forbidden for %s status", frame.Status)
		}
	default:
		return fmt.Errorf("result.status: unknown status %q", frame.Status)
	}
	for i, evidence := range frame.Evidence {
		if evidence.Hash != "" && !evidenceHash.MatchString(evidence.Hash) {
			return fmt.Errorf("result.evidence[%d].hash: expected 64 lowercase hexadecimal characters", i)
		}
		if len(evidence.Excerpt) > MaxExcerpt {
			return fmt.Errorf("result.evidence[%d].excerpt: exceeds %d bytes", i, MaxExcerpt)
		}
	}
	return nil
}

func validateError(frame *Error, hasID bool) error {
	if err := validateEnvelope("error", frame.V, frame.Seq); err != nil {
		return err
	}
	if err := validateProblem("error", frame.Code, frame.Message); err != nil {
		return err
	}
	if hasID {
		if err := validateID("error.id", frame.ID); err != nil {
			return err
		}
	}
	if frame.Protocols != nil || frame.Code == CodeUnsupportedProtocol {
		return validateProtocols("error.protocols", frame.Protocols)
	}
	return nil
}

func validateProgram(path string, program Program) error {
	if !programName.MatchString(program.Name) {
		return fmt.Errorf("%s.name: invalid name", path)
	}
	if !programVersion.MatchString(program.Version) {
		return fmt.Errorf("%s.version: invalid version", path)
	}
	return nil
}

func validateProtocols(path string, protocols []string) error {
	if len(protocols) == 0 {
		return fmt.Errorf("%s: at least one entry is required", path)
	}
	for _, version := range protocols {
		if !protocolName.MatchString(version) {
			return fmt.Errorf("%s: invalid protocol %q", path, version)
		}
	}
	return nil
}

func validateID(path, id string) error {
	if !callID.MatchString(id) {
		return fmt.Errorf("%s: invalid call ID", path)
	}
	return nil
}

func validateProblem(path, code, message string) error {
	switch code {
	case CodeUnsupportedProtocol, CodeBadRequest, CodeUnsupported, CodeBusy, CodeInternal:
	default:
		return fmt.Errorf("%s.code: unknown code %q", path, code)
	}
	if message == "" {
		return fmt.Errorf("%s.message: required", path)
	}
	return nil
}
