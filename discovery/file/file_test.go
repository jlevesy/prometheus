// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package file

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/common/model"

	"github.com/prometheus/prometheus/config"
	"github.com/prometheus/prometheus/util/testutil"
)

const testDir = "fixtures"

func TestFileSD(t *testing.T) {
	defer os.Remove(filepath.Join(testDir, "_test_valid.yml"))
	defer os.Remove(filepath.Join(testDir, "_test_valid.json"))
	defer os.Remove(filepath.Join(testDir, "_test_invalid_nil.json"))
	defer os.Remove(filepath.Join(testDir, "_test_invalid_nil.yml"))
	testFileSD(t, "valid", ".yml", true)
	testFileSD(t, "valid", ".json", true)
	testFileSD(t, "invalid_nil", ".json", false)
	testFileSD(t, "invalid_nil", ".yml", false)
}

func testFileSD(t *testing.T, prefix, ext string, expect bool) {
	// As interval refreshing is more of a fallback, we only want to test
	// whether file watches work as expected.
	var conf config.FileSDConfig
	conf.Files = []string{filepath.Join(testDir, "_*"+ext)}
	conf.RefreshInterval = model.Duration(1 * time.Hour)

	var (
		fsd         = NewDiscovery(&conf, nil)
		ch          = make(chan []*config.TargetGroup)
		ctx, cancel = context.WithCancel(context.Background())
	)
	go fsd.Run(ctx, ch)

	select {
	case <-time.After(25 * time.Millisecond):
		// Expected.
	case tgs := <-ch:
		t.Fatalf("Unexpected target groups in file discovery: %s", tgs)
	}

	// To avoid empty group struct sent from the discovery caused by invalid fsnotify updates,
	// drain the channel until we are ready with the test files.
	fileReady := make(chan struct{})
	drainReady := make(chan struct{})
	go func() {
		for {
			select {
			case <-ch:
			case <-fileReady:
				close(drainReady)
				return
			}
		}
	}()

	newf, err := os.Create(filepath.Join(testDir, "_test_"+prefix+ext))
	testutil.Ok(t, err)

	f, err := os.Open(filepath.Join(testDir, prefix+ext))
	testutil.Ok(t, err)

	defer f.Close()
	_, err = io.Copy(newf, f)
	testutil.Ok(t, err)

	// File is written with the config so stop draining the discovery channel.
	// It needs to be before the file closing so that fsnotify triggers a new loop of the discovery service.
	close(fileReady)
	<-drainReady
	newf.Close()

	timeout := time.After(15 * time.Second)
	// The files contain two target groups.
retry:
	for {
		select {
		case <-timeout:
			if expect {
				t.Fatalf("Expected new target group but got none")
			} else {
				// invalid type fsd should always broken down.
				break retry
			}
		case tgs := <-ch:
			testutil.Assert(
				t,
				expect,
				"Unexpected target groups %s, we expected a failure here.",
				tgs,
			)

			if len(tgs) != 2 {
				continue retry // Potentially a partial write, just retry.
			}
			tg := tgs[0]

			_, ok := tg.Labels["foo"]
			testutil.Assert(t, ok, "Labels not parsed")
			testutil.Equals(
				t, filepath.Join(testDir, "_test_"+prefix+ext+":0"), tg.String(),
			)

			tg = tgs[1]
			testutil.Equals(
				t, filepath.Join(testDir, "_test_"+prefix+ext+":1"), tg.String(),
			)

			break retry
		}
	}

	// Based on unknown circumstances, sometimes fsnotify will trigger more events in
	// some runs (which might be empty, chains of different operations etc.).
	// We have to drain those (as the target manager would) to avoid deadlocking and must
	// not try to make sense of it all...
	drained := make(chan struct{})
	go func() {
		for {
			select {
			case tgs := <-ch:
				// Below we will change the file to a bad syntax. Previously extracted target
				// groups must not be deleted via sending an empty target group.
				if len(tgs[0].Targets) == 0 {
					t.Errorf("Unexpected empty target groups received: %s", tgs)
				}
			case <-time.After(500 * time.Millisecond):
				close(drained)
				return
			}
		}
	}()

	newf, err = os.Create(filepath.Join(testDir, "_test.new"))
	testutil.Ok(t, err)

	defer os.Remove(newf.Name())

	_, err = newf.Write([]byte("]gibberish\n]["))

	testutil.Ok(t, err)

	newf.Close()

	os.Rename(newf.Name(), filepath.Join(testDir, "_test_"+prefix+ext))

	cancel()
	<-drained
}
