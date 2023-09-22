// Copyright 2018 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package adapter

// NOTE: you do not need to edit this file when implementing a custom sd.
import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/discovery"
	"github.com/prometheus/prometheus/discovery/targetgroup"
)

type customSD struct {
	Targets []string          `json:"targets"`
	Labels  map[string]string `json:"labels"`
}

func fingerprint(group *targetgroup.Group) model.Fingerprint {
	groupFingerprint := model.LabelSet{}.Fingerprint()
	for _, targets := range group.Targets {
		groupFingerprint ^= targets.Fingerprint()
	}
	groupFingerprint ^= group.Labels.Fingerprint()
	return groupFingerprint
}

// Adapter runs an unknown service discovery implementation and converts its target groups
// to JSON and writes to a file for file_sd.
type Adapter struct {
	ctx     context.Context
	disc    discovery.Discoverer
	groups  map[string]*customSD
	manager *discovery.Manager
	output  string
	path    string
	name    string
	logger  log.Logger
}

func mapToArray(m map[string]*customSD) []customSD {
	arr := make([]customSD, 0, len(m))
	for _, v := range m {
		arr = append(arr, *v)
	}
	return arr
}

func generateTargetGroups(allTargetGroups map[string][]*targetgroup.Group) map[string]*customSD {
	groups := make(map[string]*customSD)
	for k, sdTargetGroups := range allTargetGroups {
		for _, group := range sdTargetGroups {
			newTargets := make([]string, 0)
			newLabels := make(map[string]string)

			for _, targets := range group.Targets {
				for _, target := range targets {
					newTargets = append(newTargets, string(target))
				}
			}
			sort.Strings(newTargets)
			for name, value := range group.Labels {
				newLabels[string(name)] = string(value)
			}

			sdGroup := customSD{

				Targets: newTargets,
				Labels:  newLabels,
			}
			// Make a unique key, including group's fingerprint, in case the sd_type (map key) and group.Source is not unique.
			groupFingerprint := fingerprint(group)
			key := fmt.Sprintf("%s:%s:%s", k, group.Source, groupFingerprint.String())
			groups[key] = &sdGroup
		}
	}

	return groups
}

// Parses incoming target groups updates. If the update contains changes to the target groups
// Adapter already knows about, or new target groups, we Marshal to JSON and write to file.
func (a *Adapter) refreshTargetGroups(allTargetGroups map[string][]*targetgroup.Group) {
	tempGroups := generateTargetGroups(allTargetGroups)

	if !reflect.DeepEqual(a.groups, tempGroups) {
		a.groups = tempGroups
		err := a.writeOutput()
		if err != nil {
			level.Error(log.With(a.logger, "component", "sd-adapter")).Log("err", err)
		}
	}
}

// Writes JSON formatted targets to output file.
func (a *Adapter) writeOutput() error {
	level.Info(a.logger).Log("msg", "Inside writeOutput")
	arr := mapToArray(a.groups)
	b, _ := json.MarshalIndent(arr, "", "    ")

	dir, _ := filepath.Split(a.output)
	tmpfile, err := ioutil.TempFile(dir, "sd-adapter")
	if err != nil {
		return err
	}
	defer tmpfile.Close()

	_, err = tmpfile.Write(b)
	if err != nil {
		return err
	}

	err = os.Rename(tmpfile.Name(), a.output)
	if err != nil {
		return err
	}

	err = os.Chmod(a.output, 0777)
	if err != nil {
		return err
	}

	err = a.MoveFile(a.output, a.path+"/"+a.output)
	if err != nil {
		return err
	}

	level.Info(a.logger).Log("msg", "End of writeOutput")
	return nil
}

func (a *Adapter) MoveFile(sourcePath, destPath string) error {
	level.Info(a.logger).Log("msg", "Start of MoveFile", "sourcePath", sourcePath, "destPath", destPath)
	sourceFile, err := os.Open(sourcePath)
	if err != nil {
		return fmt.Errorf("Couldn't open source file: %s", err)
	}
	outputFile, err := os.Create(destPath)
	if err != nil {
		sourceFile.Close()
		return fmt.Errorf("Couldn't open dest file: %s", err)
	}
	defer outputFile.Close()
	_, err = io.Copy(outputFile, sourceFile)
	sourceFile.Close()
	if err != nil {
		return fmt.Errorf("Writing to output file failed: %s", err)
	}
	// The copy was successful, so now delete the original file
	err = os.Remove(sourcePath)
	if err != nil {
		return fmt.Errorf("Failed removing original file: %s", err)
	}
	level.Info(a.logger).Log("msg", "End of MoveFile")
	return nil
}

func (a *Adapter) runCustomSD(ctx context.Context) {
	updates := a.manager.SyncCh()
	for {
		select {
		case <-ctx.Done():
		case allTargetGroups, ok := <-updates:
			// Handle the case that a target provider exits and closes the channel
			// before the context is done.
			if !ok {
				return
			}
			a.refreshTargetGroups(allTargetGroups)
		}
	}
}

// Run starts a Discovery Manager and the custom service discovery implementation.
func (a *Adapter) Run() {
	go a.manager.Run()
	a.manager.StartCustomProvider(a.ctx, a.name, a.disc)
	go a.runCustomSD(a.ctx)
}

// NewAdapter creates a new instance of Adapter.
func NewAdapter(ctx context.Context, file string, path string, name string, d discovery.Discoverer, logger log.Logger) *Adapter {
	return &Adapter{
		ctx:     ctx,
		disc:    d,
		groups:  make(map[string]*customSD),
		manager: discovery.NewManager(ctx, logger),
		output:  file,
		path:    path,
		name:    name,
		logger:  logger,
	}
}
