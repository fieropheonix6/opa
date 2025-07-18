// Copyright 2019 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// Package authz contains unit and benchmark tests for authz use-cases
// The public (non-test) APIs are meant to be used as helpers for
// other tests to build off of.
package authz

import (
	"encoding/json"
	"fmt"

	"github.com/open-policy-agent/opa/v1/util"
)

// Policy is a test rego policy for a token based authz system
const Policy = `package policy.restauthz

import data.restauthz.tokens

default allow := false

allow if {
	token := tokens[input.token_id]
	some authz in token.authz_profiles
	regex.match(authz.path, input.path)
	input.method in authz.methods
}`

// AllowQuery is the test query that goes with the Policy
// defined in this package
const AllowQuery = "data.policy.restauthz.allow"

// DataSetProfile defines how the test data should be generated
type DataSetProfile struct {
	NumTokens int
	NumPaths  int
}

// InputMode defines what type of inputs to generate for testings
type InputMode int

// InputMode types supported by GenerateInput
const (
	ForbidIdentity = iota
	ForbidPath     = iota
	ForbidMethod   = iota
	Allow          = iota
)

// GenerateInput will use a dataset profile and desired InputMode to generate inputs for testing
func GenerateInput(profile DataSetProfile, mode InputMode) (any, any) {
	var input string
	var allow bool

	switch mode {
	case ForbidIdentity:
		input = fmt.Sprintf(`
		{
			"token_id": "deadbeef",
			"path": %q,
			"method": "GET"
		}
		`, generateRequestPath(profile.NumPaths-1))
	case ForbidPath:
		input = fmt.Sprintf(`
		{
			"token_id": %q,
			"path": %q,
			"method": "GET"
		}`, generateTokenID(profile.NumTokens-1), "/api/v1/resourcetype-deadbeef/deadbeefresourceid")
	case ForbidMethod:
		input = fmt.Sprintf(`
		{
			"token_id": %q,
			"path": %q,
			"method": "DEADBEEF"
		}
		`, generateTokenID(profile.NumTokens-1), generateRequestPath(profile.NumPaths-1))
	default:
		input = fmt.Sprintf(`
		{
			"token_id": %q,
			"path": %q,
			"method": "GET"
		}
		`, generateTokenID(profile.NumTokens-1), generateRequestPath(profile.NumPaths-1))
		allow = true
	}

	return util.MustUnmarshalJSON([]byte(input)), allow
}

// GenerateDataset will generate a dataset for the given DatasetProfile
func GenerateDataset(profile DataSetProfile) map[string]any {
	return map[string]any{
		"restauthz": map[string]any{
			"tokens": generateTokensJSON(profile),
		},
	}
}

func generateTokensJSON(profile DataSetProfile) any {
	tokens := generateTokens(profile)
	bs, err := json.Marshal(tokens)
	if err != nil {
		panic(err)
	}
	return util.MustUnmarshalJSON(bs)
}

type token struct {
	ID            string         `json:"id"`
	AuthzProfiles []authzProfile `json:"authz_profiles"`
}

type authzProfile struct {
	Path    string   `json:"path"`
	Methods []string `json:"methods"`
}

func generateTokens(profile DataSetProfile) map[string]token {
	tokens := map[string]token{}
	for i := range profile.NumTokens {
		token := generateToken(profile, i)
		tokens[token.ID] = token
	}
	return tokens
}

func generateToken(profile DataSetProfile, i int) token {
	return token{
		ID:            generateTokenID(i),
		AuthzProfiles: generateAuthzProfiles(profile),
	}
}

func generateAuthzProfiles(profile DataSetProfile) []authzProfile {
	profiles := make([]authzProfile, profile.NumPaths)
	for i := range profile.NumPaths {
		profiles[i] = generateAuthzProfile(profile, i)
	}
	return profiles
}

func generateAuthzProfile(_ DataSetProfile, i int) authzProfile {
	return authzProfile{
		Path: generateAuthzPath(i),
		Methods: []string{
			"POST",
			"GET",
		},
	}
}

func generateTokenID(suffix int) string {
	return fmt.Sprintf("token-%d", suffix)
}

func generateAuthzPath(i int) string {
	return fmt.Sprintf("/api/v1/resourcetype-%d/*", i)
}

func generateRequestPath(i int) string {
	return fmt.Sprintf("/api/v1/resourcetype-%d/somefakeresourceid000000111111", i)
}
