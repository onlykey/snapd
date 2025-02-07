// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2019 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */
package seccomp

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/snapcore/snapd/osutil"
)

var (
	// version-info format: <build-id> <libseccomp-version> <hash> <features>
	// Where, the hash is calculated over all syscall names supported by the
	// libseccomp library. The build-id is a 160-bit SHA-1 (40 char) string
	// and the hash is a 256-bit SHA-256 (64 char) string. Allow libseccomp
	// version to be 1-5 chars per field (eg, 1.2.3 or 12345.23456.34567)
	// and 1-30 chars of colon-separated features.
	// Ex: 7ac348ac9c934269214b00d1692dfa50d5d4a157 2.3.3 03e996919907bc7163bc83b95bca0ecab31300f20dfa365ea14047c698340e7c bpf-actlog
	validVersionInfo = regexp.MustCompile(`^[0-9a-f]{1,40} [0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5} [0-9a-f]{1,64} [-a-z0-9:]{1,30}$`)
)

type Compiler struct {
	snapSeccomp string
}

// NewCompiler returns a wrapper for the compiler binary. The path to the binary is
// looked up using the lookupTool helper.
func NewCompiler(lookupTool func(name string) (string, error)) (*Compiler, error) {
	if lookupTool == nil {
		panic("lookup tool func not provided")
	}

	path, err := lookupTool("snap-seccomp")
	if err != nil {
		return nil, err
	}

	return &Compiler{snapSeccomp: path}, nil
}

// VersionInfo returns the version information of the compiler. The format of
// version information is: <build-id> <libseccomp-version> <hash> <features>.
// Where, the hash is calculated over all syscall names supported by the
// libseccomp library.
func (c *Compiler) VersionInfo() (VersionInfo, error) {
	cmd := exec.Command(c.snapSeccomp, "version-info")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", osutil.OutputErr(output, err)
	}
	raw := bytes.TrimSpace(output)
	// Example valid output:
	// 7ac348ac9c934269214b00d1692dfa50d5d4a157 2.3.3 03e996919907bc7163bc83b95bca0ecab31300f20dfa365ea14047c698340e7c bpf-actlog
	if match := validVersionInfo.Match(raw); !match {
		return "", fmt.Errorf("invalid format of version-info: %q", raw)
	}

	return VersionInfo(raw), nil
}

var compilerVersionInfoImpl = func(lookupTool func(name string) (string, error)) (VersionInfo, error) {
	c, err := NewCompiler(lookupTool)
	if err != nil {
		return VersionInfo(""), err
	}
	return c.VersionInfo()
}

// CompilerVersionInfo returns the version information of snap-seccomp
// looked up via lookupTool.
func CompilerVersionInfo(lookupTool func(name string) (string, error)) (VersionInfo, error) {
	return compilerVersionInfoImpl(lookupTool)
}

// MockCompilerVersionInfo mocks the return value of CompilerVersionInfo.
func MockCompilerVersionInfo(versionInfo string) (restore func()) {
	old := compilerVersionInfoImpl
	compilerVersionInfoImpl = func(_ func(name string) (string, error)) (VersionInfo, error) {
		return VersionInfo(versionInfo), nil
	}
	return func() {
		compilerVersionInfoImpl = old
	}
}

var errEmptyVersionInfo = errors.New("empty version-info")

// VersionInfo represents information about the seccomp compiler
type VersionInfo string

// LibseccompVersion parses VersionInfo and provides the libseccomp version
func (vi VersionInfo) LibseccompVersion() (string, error) {
	if vi == "" {
		return "", errEmptyVersionInfo
	}
	if match := validVersionInfo.Match([]byte(vi)); !match {
		return "", fmt.Errorf("invalid format of version-info: %q", vi)
	}
	return strings.Split(string(vi), " ")[1], nil
}

// Features parses the output of VersionInfo and provides the
// golang seccomp features
func (vi VersionInfo) Features() (string, error) {
	if vi == "" {
		return "", errEmptyVersionInfo
	}
	if match := validVersionInfo.Match([]byte(vi)); !match {
		return "", fmt.Errorf("invalid format of version-info: %q", vi)
	}
	return strings.Split(string(vi), " ")[3], nil
}

// HasFeature parses the output of VersionInfo and answers whether or
// not golang-seccomp supports the feature
func (vi VersionInfo) HasFeature(feature string) (bool, error) {
	features, err := vi.Features()
	if err != nil {
		return false, err
	}
	for _, f := range strings.Split(features, ":") {
		if f == feature {
			return true, nil
		}
	}
	return false, nil
}

// BuildTimeRequirementError represents the error case of a feature
// that cannot be supported because of unfulfilled build time
// requirements.
type BuildTimeRequirementError struct {
	Feature      string
	Requirements []string
}

func (e *BuildTimeRequirementError) RequirementsString() string {
	return strings.Join(e.Requirements, ", ")
}

func (e *BuildTimeRequirementError) Error() string {
	return fmt.Sprintf("%s requires a snapd built against %s", e.Feature, e.RequirementsString())
}

// SupportsRobustArgumentFiltering parses the output of VersionInfo and
// determines if libseccomp and golang-seccomp are new enough to support robust
// argument filtering
func (vi VersionInfo) SupportsRobustArgumentFiltering() error {
	libseccompVersion, err := vi.LibseccompVersion()
	if err != nil {
		return err
	}

	// Parse <libseccomp version>
	tmp := strings.Split(libseccompVersion, ".")
	maj, err := strconv.Atoi(tmp[0])
	if err != nil {
		return fmt.Errorf("cannot obtain seccomp compiler information: %v", err)
	}
	min, err := strconv.Atoi(tmp[1])
	if err != nil {
		return fmt.Errorf("cannot obtain seccomp compiler information: %v", err)
	}

	var unfulfilledReqs []string

	// libseccomp < 2.4 has significant argument filtering bugs that we
	// cannot reliably work around with this feature.
	if maj < 2 || (maj == 2 && min < 4) {
		unfulfilledReqs = append(unfulfilledReqs, "libseccomp >= 2.4")
	}

	// Due to https://github.com/seccomp/libseccomp-golang/issues/22,
	// golang-seccomp <= 0.9.0 cannot create correct BPFs for this feature.
	// The package does not contain any version information, but we know
	// that ActLog was implemented in the library after this issue was
	// fixed, so base the decision on that. ActLog is first available in
	// 0.9.1.
	res, err := vi.HasFeature("bpf-actlog")
	if err != nil {
		return err
	}
	if !res {
		unfulfilledReqs = append(unfulfilledReqs, "golang-seccomp >= 0.9.1")
	}

	if len(unfulfilledReqs) != 0 {
		return &BuildTimeRequirementError{
			Feature:      "robust argument filtering",
			Requirements: unfulfilledReqs,
		}
	}

	return nil
}

// Compile compiles given source profile and saves the result to the out
// location.
func (c *Compiler) Compile(in, out string) error {
	cmd := exec.Command(c.snapSeccomp, "compile", in, out)
	if output, err := cmd.CombinedOutput(); err != nil {
		return osutil.OutputErr(output, err)
	}
	return nil
}
