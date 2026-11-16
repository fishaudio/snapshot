// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package podcontract

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestRestoreInputsFromAnnotations(t *testing.T) {
	t.Run("absent asks for nothing", func(t *testing.T) {
		got, err := RestoreInputsFromAnnotations(map[string]string{RestoreFromAnnotation: "snapshot-a"})
		if err != nil || got != nil {
			t.Fatalf("RestoreInputsFromAnnotations() = %v, %v; want nil, nil", got, err)
		}
	})

	t.Run("reads every destination", func(t *testing.T) {
		got, err := RestoreInputsFromAnnotations(map[string]string{
			RestoreMatchInputsAnnotation: `{"prefill": {"env": ["DB_PASSWORD", "API_URL"], "args": true},
				"decode": {"env": ["DB_PASSWORD"]}, "router": {"args": true}}`,
		})
		if err != nil {
			t.Fatalf("RestoreInputsFromAnnotations(): %v", err)
		}
		want := map[string]RestoreInputs{
			"prefill": {Env: []string{"DB_PASSWORD", "API_URL"}, Args: true},
			"decode":  {Env: []string{"DB_PASSWORD"}},
			"router":  {Args: true},
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("RestoreInputsFromAnnotations() = %#v, want %#v", got, want)
		}
	})

	// Anything not read in full is refused: an unknown key may ask for a
	// check this version cannot make, and a misread one would compare less
	// than its writer asked for.
	for name, tc := range map[string]struct {
		value   string
		wantErr string
	}{
		"empty":                  {value: ``, wantErr: "not a JSON object"},
		"not JSON":               {value: `main=DB_PASSWORD`, wantErr: "not a JSON object"},
		"null":                   {value: `null`, wantErr: "not a JSON object"},
		"an array":               {value: `[{"env": ["A"]}]`, wantErr: "not a JSON object"},
		"no destination":         {value: `{}`, wantErr: "names no destination container"},
		"two values":             {value: `{"main": {"args": true}} {}`, wantErr: "not a JSON object"},
		"entry is null":          {value: `{"main": null}`, wantErr: `entry "main" is not a JSON object`},
		"entry is a list":        {value: `{"main": ["A"]}`, wantErr: "not a JSON object"},
		"entry asks for nothing": {value: `{"main": {}}`, wantErr: "names no input to compare"},
		"only false args":        {value: `{"main": {"args": false}}`, wantErr: "names no input to compare"},
		"empty env":              {value: `{"main": {"env": []}}`, wantErr: "names no input to compare"},
		"null env":               {value: `{"main": {"env": null}}`, wantErr: `key "env": is null`},
		"null args":              {value: `{"main": {"env": ["A"], "args": null}}`, wantErr: `key "args": is null`},
		"args as a string":       {value: `{"main": {"args": "true"}}`, wantErr: `key "args"`},
		"env as a string":        {value: `{"main": {"env": "A"}}`, wantErr: `key "env"`},
		"unknown input": {
			value:   `{"main": {"env": ["A"], "files": ["/etc/app"]}}`,
			wantErr: `key "files": is not a known input`,
		},
		"key in another case": {value: `{"main": {"ENV": ["A"]}}`, wantErr: `key "ENV": is not a known input`},
		"destination twice": {
			value:   `{"main": {"env": ["A"]}, "main": {"args": true}}`,
			wantErr: `key "main" appears twice`,
		},
		"input twice":            {value: `{"main": {"env": ["A"], "env": ["B"]}}`, wantErr: `key "env" appears twice`},
		"invalid destination":    {value: `{"Main_1": {"env": ["A"]}}`, wantErr: "invalid destination container"},
		"empty variable name":    {value: `{"main": {"env": [""]}}`, wantErr: "invalid environment variable name"},
		"variable name with =":   {value: `{"main": {"env": ["A=B"]}}`, wantErr: "invalid environment variable name"},
		"variable name with NUL": {value: `{"main": {"env": ["A\u0000"]}}`, wantErr: "invalid environment variable name"},
		"variable named twice": {
			value:   `{"main": {"env": ["A", "B", "A"]}}`,
			wantErr: `names environment variable "A" twice`,
		},
		"variable name not a string": {value: `{"main": {"env": [1]}}`, wantErr: `key "env"`},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := RestoreInputsFromAnnotations(map[string]string{RestoreMatchInputsAnnotation: tc.value})
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("RestoreInputsFromAnnotations() = %v, %v; want an error containing %q", got, err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), RestoreMatchInputsAnnotation) {
				t.Fatalf("error %q does not name %s", err, RestoreMatchInputsAnnotation)
			}
		})
	}

	t.Run("at most MaxRestoreInputEnvNames variables", func(t *testing.T) {
		names := make([]string, MaxRestoreInputEnvNames+1)
		for i := range names {
			names[i] = fmt.Sprintf(`"VAR_%d"`, i)
		}
		value := `{"main": {"env": [` + strings.Join(names, ",") + `]}}`
		if _, err := RestoreInputsFromAnnotations(map[string]string{RestoreMatchInputsAnnotation: value}); err == nil ||
			!strings.Contains(err.Error(), "the limit is") {
			t.Fatalf("RestoreInputsFromAnnotations() error = %v, want the limit", err)
		}
		value = `{"main": {"env": [` + strings.Join(names[:MaxRestoreInputEnvNames], ",") + `]}}`
		if _, err := RestoreInputsFromAnnotations(map[string]string{RestoreMatchInputsAnnotation: value}); err != nil {
			t.Fatalf("RestoreInputsFromAnnotations() with %d names: %v", MaxRestoreInputEnvNames, err)
		}
	})

	t.Run("nested duplicates are found past other values", func(t *testing.T) {
		value := `{"a": {"env": ["X"]}, "b": {"env": ["Y"], "args": true}, "c": {"args": true, "args": false}}`
		_, err := RestoreInputsFromAnnotations(map[string]string{RestoreMatchInputsAnnotation: value})
		if err == nil || !strings.Contains(err.Error(), `key "args" appears twice`) {
			t.Fatalf("error = %v, want the repeated args key", err)
		}
	})
}

func TestValidateRestoreInputsDestinations(t *testing.T) {
	entries := map[string]RestoreInputs{"prefill": {Args: true}, "decode": {Env: []string{"A"}}}
	if err := ValidateRestoreInputsDestinations(entries, []string{"prefill", "decode", "router"}); err != nil {
		t.Fatalf("ValidateRestoreInputsDestinations(): %v", err)
	}
	if err := ValidateRestoreInputsDestinations(nil, []string{"main"}); err != nil {
		t.Fatalf("ValidateRestoreInputsDestinations(nil): %v", err)
	}
	err := ValidateRestoreInputsDestinations(entries, []string{"prefill"})
	if err == nil || !strings.Contains(err.Error(), `"decode", which is not a restore destination`) {
		t.Fatalf("ValidateRestoreInputsDestinations() error = %v, want decode named", err)
	}
}
