/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package tlsprofile provides TLS profile resolution from OpenShift APIServer
// and helpers to apply it to controller-runtime TLS servers.
package tlsprofile

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"

	configv1 "github.com/openshift/api/config/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// APIServerName is the name of the APIServer resource in the cluster.
	APIServerName = "cluster"
)

var (
	// ErrCustomProfileNil is returned when a custom TLS profile is specified but the Custom field is nil.
	ErrCustomProfileNil = errors.New("custom TLS profile specified but Custom field is nil")
)

// FetchAPIServerTLSProfile fetches the TLS profile spec configured in APIServer.
// If the APIServer resource is not found (e.g. non-OpenShift), returns the default (Intermediate) profile and nil error.
func FetchAPIServerTLSProfile(ctx context.Context, k8sClient client.Client) (configv1.TLSProfileSpec, error) {
	apiServer := &configv1.APIServer{}
	key := client.ObjectKey{Name: APIServerName}

	if err := k8sClient.Get(ctx, key, apiServer); err != nil {
		if client.IgnoreNotFound(err) == nil {
			// APIServer not found (e.g. vanilla Kubernetes); use default profile.
			defaultProfile := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
			return defaultProfile, nil
		}
		return configv1.TLSProfileSpec{}, fmt.Errorf("failed to get APIServer %q: %w", key.String(), err)
	}

	return GetTLSProfileSpec(apiServer.Spec.TLSSecurityProfile)
}

// GetTLSProfileSpec returns TLSProfileSpec for the given profile.
// If no profile is configured, the default (Intermediate) profile is returned.
func GetTLSProfileSpec(profile *configv1.TLSSecurityProfile) (configv1.TLSProfileSpec, error) {
	defaultProfile := *configv1.TLSProfiles[configv1.TLSProfileIntermediateType]
	if profile == nil || profile.Type == "" {
		return defaultProfile, nil
	}

	profileType := profile.Type
	if profileType != configv1.TLSProfileCustomType {
		if tlsConfig, ok := configv1.TLSProfiles[profileType]; ok {
			return *tlsConfig, nil
		}
		return defaultProfile, nil
	}

	if profile.Custom == nil {
		return configv1.TLSProfileSpec{}, ErrCustomProfileNil
	}
	return profile.Custom.TLSProfileSpec, nil
}

var (
	cipherSuiteByName map[string]uint16
	cipherSuiteOnce   sync.Once
)

func initCipherMap() {
	cipherSuiteByName = make(map[string]uint16)
	for _, cs := range tls.CipherSuites() {
		cipherSuiteByName[cs.Name] = cs.ID
	}
}

// cipherCode returns the TLS cipher code for an IANA cipher name, or 0 if not supported.
func cipherCode(cipher string) uint16 {
	cipherSuiteOnce.Do(initCipherMap)
	if code, ok := cipherSuiteByName[cipher]; ok {
		return code
	}
	return 0
}

// cipherCodes converts a list of cipher names to their uint16 codes.
// Returns the converted codes and a list of any unsupported cipher names.
func cipherCodes(ciphers []string) (codes []uint16, unsupportedCiphers []string) {
	for _, cipher := range ciphers {
		code := cipherCode(cipher)
		if code == 0 {
			unsupportedCiphers = append(unsupportedCiphers, cipher)
			continue
		}
		codes = append(codes, code)
	}
	return codes, unsupportedCiphers
}

// minTLSVersion maps configv1.TLSProtocolVersion to crypto/tls version.
func minTLSVersion(ver configv1.TLSProtocolVersion) uint16 {
	switch ver {
	case configv1.VersionTLS10:
		return tls.VersionTLS10
	case configv1.VersionTLS11:
		return tls.VersionTLS11
	case configv1.VersionTLS12:
		return tls.VersionTLS12
	case configv1.VersionTLS13:
		return tls.VersionTLS13
	default:
		return tls.VersionTLS12
	}
}

// NewTLSConfigFromProfile returns a function that configures a tls.Config based on the provided TLSProfileSpec.
// The returned function is intended to be used with controller-runtime's TLSOpts.
// CipherSuites are only set when MinVersion is below TLS 1.3 (Go's TLS 1.3 does not allow configuring cipher suites).
func NewTLSConfigFromProfile(profile configv1.TLSProfileSpec) (tlsConfig func(*tls.Config), unsupportedCiphers []string) {
	minVer := minTLSVersion(configv1.TLSProtocolVersion(profile.MinTLSVersion))
	cipherSuites, unsupportedCiphers := cipherCodes(profile.Ciphers)

	return func(c *tls.Config) {
		c.MinVersion = minVer
		if minVer != tls.VersionTLS13 {
			c.CipherSuites = cipherSuites
		}
	}, unsupportedCiphers
}
