// Copyright 2017 The go-ethereum Authors
// This file is part of go-ethereum.
//
// go-ethereum is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// go-ethereum is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with go-ethereum. If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/tests"
	"github.com/urfave/cli/v2"
)

var (
	forkFlag = &cli.StringFlag{
		Name:     "statetest.fork",
		Usage:    "Only run tests for the specified fork.",
		Category: flags.VMCategory,
	}
	idxFlag = &cli.IntFlag{
		Name:     "statetest.index",
		Usage:    "The index of the subtest to run.",
		Category: flags.VMCategory,
		Value:    -1, // default to select all subtest indices
	}
	sharedMemoryFlag = &cli.StringFlag{
		Name:     "statetest.shared-memory",
		Usage:    "Run as a long-lived server: poll <file>.signal for START/EXIT, read test from <file>, write state root or error back to <file>, then write OK/FAIL to <file>.signal.",
		Category: flags.VMCategory,
	}
)
var stateTestCommand = &cli.Command{
	Action:    stateTestCmd,
	Name:      "statetest",
	Usage:     "Executes the given state tests. Filenames can be fed via standard input (batch mode) or as an argument (one-off execution).",
	ArgsUsage: "<file>",
	Flags: slices.Concat([]cli.Flag{
		BenchFlag,
		DumpFlag,
		forkFlag,
		HumanReadableFlag,
		idxFlag,
		RunFlag,
		sharedMemoryFlag,
	}, traceFlags),
}

func stateTestCmd(ctx *cli.Context) error {
	if shared := ctx.String(sharedMemoryFlag.Name); shared != "" {
		return runSharedMemoryServer(ctx, shared)
	}
	path := ctx.Args().First()

	// If path is provided, run the tests at that path.
	if len(path) != 0 {
		var (
			collected = collectFiles(path)
			results   []testResult
		)
		for _, fname := range collected {
			r, err := runStateTest(ctx, fname)
			if err != nil {
				return err
			}
			results = append(results, r...)
		}
		report(ctx, results)
		return nil
	}
	// Otherwise, read filenames from stdin and execute back-to-back.
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		fname := scanner.Text()
		if len(fname) == 0 {
			return nil
		}
		results, err := runStateTest(ctx, fname)
		if err != nil {
			return err
		}
		report(ctx, results)
	}
	return nil
}

// runStateTest loads the state-test given by fname, and executes the test.
func runStateTest(ctx *cli.Context, fname string) ([]testResult, error) {
	src, err := os.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	var testsByName map[string]tests.StateTest
	if err := json.Unmarshal(src, &testsByName); err != nil {
		return nil, fmt.Errorf("unable to read test file %s: %w", fname, err)
	}

	cfg := vm.Config{Tracer: tracerFromFlags(ctx)}
	re, err := regexp.Compile(ctx.String(RunFlag.Name))
	if err != nil {
		return nil, fmt.Errorf("invalid regex -%s: %v", RunFlag.Name, err)
	}

	// Iterate over all the tests, run them and aggregate the results
	results := make([]testResult, 0, len(testsByName))
	for key, test := range testsByName {
		if !re.MatchString(key) {
			continue
		}
		for i, st := range test.Subtests() {
			if idx := ctx.Int(idxFlag.Name); idx != -1 && idx != i {
				// If specific index requested, skip all tests that do not match.
				continue
			}
			if fork := ctx.String(forkFlag.Name); fork != "" && st.Fork != fork {
				// If specific fork requested, skip all tests that do not match.
				continue
			}
			// Run the test and aggregate the result
			result := &testResult{Name: key, Fork: st.Fork, Pass: true}
			test.Run(st, cfg, false, rawdb.HashScheme, func(err error, state *tests.StateTestState) {
				var root common.Hash
				if state.StateDB != nil {
					root = state.StateDB.IntermediateRoot(false)
					result.Root = &root
					fmt.Fprintf(os.Stderr, "{\"stateRoot\": \"%#x\"}\n", root)
					// Dump any state to aid debugging.
					if ctx.Bool(DumpFlag.Name) {
						result.State = dump(state.StateDB)
					}
				}
				// Collect bench stats if requested.
				if ctx.Bool(BenchFlag.Name) {
					_, stats, _ := timedExec(true, func() ([]byte, uint64, error) {
						_, _, gasUsed, _ := test.RunNoVerify(st, cfg, false, rawdb.HashScheme)
						return nil, gasUsed, nil
					})
					result.Stats = &stats
				}
				if err != nil {
					// Test failed, mark as so.
					result.Pass, result.Error = false, err.Error()
					return
				}
			})
			results = append(results, *result)
		}
	}
	return results, nil
}

// runSharedMemoryServer parks the binary in a poll loop coordinated with a
// harness via two files: dataPath holds the test JSON (then the result), and
// dataPath+".signal" holds one of START/OK/FAIL/EXIT. See the package-level
// docs / cmd help for the exact wire protocol.
func runSharedMemoryServer(ctx *cli.Context, dataPath string) error {
	signalPath := dataPath + ".signal"

	cfg := vm.Config{Tracer: tracerFromFlags(ctx)}
	re, err := regexp.Compile(ctx.String(RunFlag.Name))
	if err != nil {
		return fmt.Errorf("invalid regex -%s: %v", RunFlag.Name, err)
	}
	forkFilter := ctx.String(forkFlag.Name)
	idxFilter := ctx.Int(idxFlag.Name)

	for {
		signal := readSignal(signalPath)
		switch signal {
		case "EXIT":
			return nil
		case "START":
			rootHex, logsHex, runErr := runStateTestForServer(dataPath, cfg, re, forkFilter, idxFilter)
			if runErr != nil {
				if werr := atomicWrite(dataPath, []byte(runErr.Error())); werr != nil {
					return fmt.Errorf("failed to write error to %s: %w", dataPath, werr)
				}
				if werr := atomicWrite(signalPath, []byte("FAIL")); werr != nil {
					return fmt.Errorf("failed to write signal: %w", werr)
				}
			} else {
				if werr := atomicWrite(dataPath, []byte(rootHex+"\n"+logsHex+"\n")); werr != nil {
					return fmt.Errorf("failed to write root to %s: %w", dataPath, werr)
				}
				if werr := atomicWrite(signalPath, []byte("OK")); werr != nil {
					return fmt.Errorf("failed to write signal: %w", werr)
				}
			}
		default:
			time.Sleep(100 * time.Microsecond)
		}
	}
}

// readSignal returns the trimmed contents of the signal file, or "" if it
// can't be read (treated as "no signal yet").
func readSignal(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// atomicWrite writes data to a sibling .tmp file then renames over path. The
// rename is atomic on POSIX filesystems.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// runStateTestForServer parses the test JSON at dataPath, runs every subtest
// matching the supplied filters, and returns the single post-state root hex
// and logs-hash hex. Errors if zero or multiple distinct (root, logs) pairs are produced.
func runStateTestForServer(dataPath string, cfg vm.Config, re *regexp.Regexp, forkFilter string, idxFilter int) (string, string, error) {
	src, err := os.ReadFile(dataPath)
	if err != nil {
		return "", "", err
	}
	var testsByName map[string]tests.StateTest
	if err := json.Unmarshal(src, &testsByName); err != nil {
		return "", "", fmt.Errorf("unable to parse test file %s: %w", dataPath, err)
	}

	var (
		gotRoot common.Hash
		gotLogs common.Hash
		seen    bool
	)
	for key, test := range testsByName {
		if !re.MatchString(key) {
			continue
		}
		for i, st := range test.Subtests() {
			if idxFilter != -1 && idxFilter != i {
				continue
			}
			if forkFilter != "" && st.Fork != forkFilter {
				continue
			}
			var (
				subRoot  common.Hash
				subLogs  common.Hash
				captured bool
			)
			test.Run(st, cfg, false, rawdb.HashScheme, func(_ error, state *tests.StateTestState) {
				if state.StateDB != nil {
					subRoot = state.StateDB.IntermediateRoot(false)
					encoded, _ := rlp.EncodeToBytes(state.StateDB.Logs())
					subLogs = crypto.Keccak256Hash(encoded)
					captured = true
				}
			})
			if !captured {
				return "", "", fmt.Errorf("subtest %s[%d] produced no state root", st.Fork, i)
			}
			if seen && (subRoot != gotRoot || subLogs != gotLogs) {
				return "", "", fmt.Errorf("multiple subtests with differing roots — please filter input")
			}
			gotRoot = subRoot
			gotLogs = subLogs
			seen = true
		}
	}
	if !seen {
		return "", "", fmt.Errorf("no matching subtest")
	}
	return fmt.Sprintf("%#x", gotRoot), fmt.Sprintf("%#x", gotLogs), nil
}
