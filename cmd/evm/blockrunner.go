// Copyright 2023 The go-ethereum Authors
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
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/internal/flags"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/tests"
	"github.com/urfave/cli/v2"
)

var blockSharedMemoryFlag = &cli.StringFlag{
	Name:     "blocktest.shared-memory",
	Usage:    "Run as a long-lived server: poll <file>.signal for START/EXIT, read block test from <file>, write one IMPORTED/REJECTED line per block or error back to <file>, then write OK/FAIL to <file>.signal.",
	Category: flags.VMCategory,
}

var blockTestCommand = &cli.Command{
	Action:    blockTestCmd,
	Name:      "blocktest",
	Usage:     "Executes the given blockchain tests. Filenames can be fed via standard input (batch mode) or as an argument (one-off execution).",
	ArgsUsage: "<path>",
	Flags: slices.Concat([]cli.Flag{
		DumpFlag,
		HumanReadableFlag,
		RunFlag,
		WitnessCrossCheckFlag,
		FuzzFlag,
		blockSharedMemoryFlag,
	}, traceFlags),
}

func blockTestCmd(ctx *cli.Context) error {
	if shared := ctx.String(blockSharedMemoryFlag.Name); shared != "" {
		return runBlockSharedMemoryServer(ctx, shared)
	}
	path := ctx.Args().First()

	// If path is provided, run the tests at that path.
	if len(path) != 0 {
		var (
			collected = collectFiles(path)
			results   []testResult
		)
		for _, fname := range collected {
			r, err := runBlockTest(ctx, fname)
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
		results, err := runBlockTest(ctx, fname)
		if err != nil {
			return err
		}
		// During fuzzing, we report the result after every block
		if !ctx.IsSet(FuzzFlag.Name) {
			report(ctx, results)
		}
	}
	return nil
}

func runBlockSharedMemoryServer(ctx *cli.Context, dataPath string) error {
	signalPath := dataPath + ".signal"
	re, err := regexp.Compile(ctx.String(RunFlag.Name))
	if err != nil {
		return fmt.Errorf("invalid regex -%s: %v", RunFlag.Name, err)
	}

	for {
		switch readSignal(signalPath) {
		case "EXIT":
			return nil
		case "START":
			statuses, runErr := runBlockTestForServer(ctx, dataPath, re)
			if runErr != nil {
				if werr := atomicWrite(dataPath, []byte(runErr.Error())); werr != nil {
					return fmt.Errorf("failed to write error to %s: %w", dataPath, werr)
				}
				if werr := atomicWrite(signalPath, []byte("FAIL")); werr != nil {
					return fmt.Errorf("failed to write signal: %w", werr)
				}
			} else {
				payload := serializeBlockStatuses(statuses)
				if werr := atomicWrite(dataPath, []byte(payload)); werr != nil {
					return fmt.Errorf("failed to write statuses to %s: %w", dataPath, werr)
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

func runBlockTestForServer(ctx *cli.Context, fname string, re *regexp.Regexp) ([]tests.BlockImportStatus, error) {
	src, err := os.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	var testsByName map[string]*tests.BlockTest
	if err := json.Unmarshal(src, &testsByName); err != nil {
		return nil, fmt.Errorf("unable to parse test file %s: %w", fname, err)
	}

	if ctx.IsSet(FuzzFlag.Name) {
		log.SetDefault(log.NewLogger(log.DiscardHandler()))
	}

	var (
		agreed []tests.BlockImportStatus
		seen   bool
	)
	matched := 0
	for _, name := range slices.Sorted(maps.Keys(testsByName)) {
		if !re.MatchString(name) {
			continue
		}
		statuses, err := testsByName[name].StatusSequence(
			false,
			rawdb.PathScheme,
			ctx.Bool(WitnessCrossCheckFlag.Name),
			tracerFromFlags(ctx),
		)
		if err != nil {
			return nil, fmt.Errorf("unable to run block test %s: %w", name, err)
		}
		if !seen {
			agreed = statuses
			seen = true
		} else if !slices.Equal(agreed, statuses) {
			return nil, fmt.Errorf("multiple block tests with differing outputs -- please filter input")
		}
		matched++
	}
	if matched == 0 {
		return nil, fmt.Errorf("no matching block test")
	}
	return agreed, nil
}

func serializeBlockStatuses(statuses []tests.BlockImportStatus) string {
	lines := make([]string, len(statuses))
	for i, status := range statuses {
		lines[i] = string(status)
	}
	return strings.Join(lines, "\n") + "\n"
}

func runBlockTest(ctx *cli.Context, fname string) ([]testResult, error) {
	src, err := os.ReadFile(fname)
	if err != nil {
		return nil, err
	}
	var tests map[string]*tests.BlockTest
	if err = json.Unmarshal(src, &tests); err != nil {
		return nil, err
	}
	re, err := regexp.Compile(ctx.String(RunFlag.Name))
	if err != nil {
		return nil, fmt.Errorf("invalid regex -%s: %v", RunFlag.Name, err)
	}
	tracer := tracerFromFlags(ctx)

	// Suppress INFO logs during fuzzing
	if ctx.IsSet(FuzzFlag.Name) {
		log.SetDefault(log.NewLogger(log.DiscardHandler()))
	}

	// Pull out keys to sort and ensure tests are run in order.
	keys := slices.Sorted(maps.Keys(tests))

	// Run all the tests.
	var results []testResult
	for _, name := range keys {
		if !re.MatchString(name) {
			continue
		}
		test := tests[name]
		result := &testResult{Name: name, Pass: true}
		var finalRoot *common.Hash
		if err := test.Run(false, rawdb.PathScheme, ctx.Bool(WitnessCrossCheckFlag.Name), tracer, func(res error, chain *core.BlockChain) {
			if ctx.Bool(DumpFlag.Name) {
				if s, _ := chain.State(); s != nil {
					result.State = dump(s)
				}
			}
			// Capture final state root for end marker
			if chain != nil {
				root := chain.CurrentBlock().Root
				finalRoot = &root
			}
		}); err != nil {
			result.Pass, result.Error = false, err.Error()
		}

		// Always assign fork (regardless of pass/fail or tracer)
		result.Fork = test.Network()
		// Assign root if test succeeded
		if result.Pass && finalRoot != nil {
			result.Root = finalRoot
		}

		// When fuzzing, write results after every block
		if ctx.IsSet(FuzzFlag.Name) {
			report(ctx, []testResult{*result})
		}
		results = append(results, *result)
	}
	return results, nil
}
