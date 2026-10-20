package config

import (
	"bytes"
	"encoding/json"
)

// MergePatch applies an RFC 7396 object or null patch and strictly decodes its result.
func MergePatch[T any](current T, raw []byte) (T, error) {
	var result T
	var patch, original map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil || patch == nil {
		return result, err
	}
	data, err := json.Marshal(current)
	if err != nil {
		return result, err
	}
	if err := json.Unmarshal(data, &original); err != nil {
		return result, err
	}
	data, err = json.Marshal(MergePatchObject(original, patch))
	if err != nil {
		return result, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	err = dec.Decode(&result)
	return result, err
}

// MergePatchObject applies RFC 7396 in place, preserving untouched Go values.
func MergePatchObject(dst, patch map[string]any) map[string]any {
	if dst == nil {
		dst = map[string]any{}
	}
	for k, v := range patch {
		switch pv := v.(type) {
		case nil:
			delete(dst, k)
		case map[string]any:
			cur, _ := dst[k].(map[string]any)
			dst[k] = MergePatchObject(cur, pv)
		default:
			dst[k] = v
		}
	}
	return dst
}
