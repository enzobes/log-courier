/*
 * Copyright 2014-2015 Jason Woods.
 *
 * This file is a modification of code from Logstash Forwarder.
 * Copyright 2012-2013 Jordan Sissel and contributors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package registrar

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/driskell/log-courier/lc-lib/core"
)

// LoadPreviousFunc is a callback implemented by a consumer of the Registrar,
// and is called for each part of a loaded previous state when LoadPrevious is
// called
type LoadPreviousFunc func(string, *FileState) (core.Stream, error)

// Registrator is the interface implemented by a Registrar implementation
type Registrator interface {
	core.IPipelineSegment

	// Connect returns a connected EventSpooler associated with the Registrar that
	// collects registrar events and sends them to the registrar. EventSpoolers
	// are connected to the registrar and the registrar will delay any shutdown
	// until all connected EventSpoolers have been disconnected
	Connect() EventSpooler

	// LoadPrevious loads the previous state from the file and calls the
	// callbackFunc for each entry
	LoadPrevious(LoadPreviousFunc) (bool, error)
}

// Registrar persists file offsets to a file that can be read again on startup
// to resume where we left off
type Registrar struct {
	core.PipelineSegment

	sync.Mutex

	registrarChan chan []EventProcessor
	references    int
	persistdir    string
	statefile     string
	state         map[core.Stream]*FileState
}

// NewRegistrar creates a new Registrar associated with a file in a directory
func NewRegistrar(app *core.App) *Registrar {
	ret := &Registrar{
		registrarChan: make(chan []EventProcessor, 16), // TODO: Make configurable?
		persistdir:    app.Config().General().PersistDir,
		statefile:     ".log-courier",
		state:         make(map[core.Stream]*FileState),
	}

	return ret
}

// LoadPrevious loads the previous state from the file
func (r *Registrar) LoadPrevious(callbackFunc LoadPreviousFunc) (havePrevious bool, err error) {
	data := make(map[string]*FileState)

	// Load the previous state - opening RDWR ensures we can write too and fail early
	// c_filename is what we will use to test create capability
	filename := r.persistdir + string(os.PathSeparator) + ".log-courier"
	newFilename := r.persistdir + string(os.PathSeparator) + ".log-courier.new"

	var f *os.File
	f, err = os.OpenFile(filename, os.O_RDWR, 0600)
	if err != nil {
		// Fail immediately if this is not a path not found error
		if !os.IsNotExist(err) {
			return
		}

		// Try the .new file - maybe we failed mid-move
		filename = newFilename
		f, err = os.OpenFile(filename, os.O_RDWR, 0600)
	}

	if err != nil {
		// Did we fail, or did it just not exist?
		if !os.IsNotExist(err) {
			return
		}
		return false, nil
	}

	// Parse the data
	log.Notice("Loading registrar data from %s", filename)
	havePrevious = true

	decoder := json.NewDecoder(f)
	decoder.Decode(&data)
	f.Close()

	r.state = make(map[core.Stream]*FileState, len(data))

	var stream core.Stream
	for file, state := range data {
		if stream, err = callbackFunc(file, state); err != nil {
			return
		}
		r.state[stream] = state
	}

	// Test we can successfully save new states by attempting to save now
	if err = r.writeRegistry(); err != nil {
		return false, fmt.Errorf("Registry write failed: %s", err)
	}

	return
}

// Connect returns a connected EventSpooler
func (r *Registrar) Connect() EventSpooler {
	r.Lock()
	defer r.Unlock()
	r.references++
	return newEventSpool(r)
}

func (r *Registrar) dereferenceSpooler() {
	r.Lock()
	defer r.Unlock()
	r.references--
	if r.references == 0 {
		// Shutdown registrar, all references are closed
		close(r.registrarChan)
	}
}

func (r *Registrar) toCanonical() (canonical map[string]*FileState) {
	canonical = make(map[string]*FileState, len(r.state))
	for _, value := range r.state {
		if _, ok := canonical[*value.Source]; ok {
			// We should never allow this - report an error
			log.Errorf("BUG: Unexpected registrar conflict detected for %s", *value.Source)
		}
		canonical[*value.Source] = value
	}
	return
}

// Run starts the registrar - it is called by the pipeline
func (r *Registrar) Run() {
	defer func() {
		r.Done()
	}()

RegistrarLoop:
	for {
		// Ignore shutdown channel - wait for registrar to close
		select {
		case spool := <-r.registrarChan:
			if spool == nil {
				break RegistrarLoop
			}

			for _, event := range spool {
				event.Process(r.state)
			}

			if err := r.writeRegistry(); err != nil {
				log.Errorf("Registry write failed: %s", err)
			}
		}
	}

	log.Info("Registrar exiting")
}
