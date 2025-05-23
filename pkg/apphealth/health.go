/*
Copyright 2022 The Dapr Authors
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

package apphealth

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"

	"k8s.io/utils/clock"

	"github.com/dapr/dapr/pkg/config"
	"github.com/dapr/kit/logger"
)

var log = logger.NewLogger("dapr.apphealth")

// AppHealth manages the health checks for the app.
type AppHealth struct {
	config       config.AppHealthConfig
	probeFn      ProbeFunction
	changeCb     ChangeCallback
	report       chan *Status
	failureCount atomic.Int32
	queue        chan struct{}

	// lastReport is the last report as UNIX microseconds time.
	lastReport atomic.Int64

	clock   clock.WithTicker
	wg      sync.WaitGroup
	closed  atomic.Bool
	closeCh chan struct{}
}

// ProbeFunction is the signature of the function that performs health probes.
// Health probe functions return errors only in case of internal errors.
// Network errors are considered probe failures, and should return nil as errors.
type ProbeFunction func(context.Context) (*Status, error)

// ChangeCallback is the signature of the callback that is invoked when the app's health status changes.
type ChangeCallback func(ctx context.Context, status *Status)

// New creates a new AppHealth object.
func New(config config.AppHealthConfig, probeFn ProbeFunction) *AppHealth {
	a := &AppHealth{
		config:  config,
		probeFn: probeFn,
		report:  make(chan *Status, 1),
		queue:   make(chan struct{}, 1),
		clock:   &clock.RealClock{},
		closeCh: make(chan struct{}),
	}

	// Initial state is unhealthy until we validate it
	a.failureCount.Store(config.Threshold)

	return a
}

// OnHealthChange sets the callback that is invoked when the health of the app changes (app becomes either healthy or unhealthy).
func (h *AppHealth) OnHealthChange(cb ChangeCallback) {
	h.changeCb = cb
}

// StartProbes starts polling the app on the interval.
func (h *AppHealth) StartProbes(ctx context.Context) error {
	if h.closed.Load() {
		return errors.New("app health is closed")
	}

	if h.probeFn == nil {
		return errors.New("cannot start probes with nil probe function")
	}
	if h.config.ProbeInterval <= 0 {
		return errors.New("probe interval must be larger than 0")
	}
	if h.config.ProbeTimeout > h.config.ProbeInterval {
		return errors.New("app health checks probe timeouts must be smaller than probe intervals")
	}

	log.Info("App health probes starting")

	ctx, cancel := context.WithCancel(ctx)

	h.wg.Add(2)
	go func() {
		defer h.wg.Done()
		defer cancel()
		select {
		case <-h.closeCh:
		case <-ctx.Done():
		}
	}()

	go func() {
		defer h.wg.Done()

		ticker := h.clock.NewTicker(h.config.ProbeInterval)
		ch := ticker.C()
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				log.Info("App health probes stopping")
				return
			case status := <-h.report:
				log.Debug("Received health status report")
				h.setResult(ctx, status)
			case <-ch:
				log.Debug("Probing app health")
				h.Enqueue()
			case <-h.queue:
				// Run synchronously so the loop is blocked
				h.doProbe(ctx)
			}
		}
	}()

	return nil
}

// Enqueue adds a new probe request to the queue
func (h *AppHealth) Enqueue() {
	// The queue has a capacity of 1, so no more than one iteration can be queued up
	select {
	case h.queue <- struct{}{}:
		// Do nothing
	default:
		// Do nothing
	}
	return
}

// ReportHealth is used by the runtime to report a health signal from the app.
func (h *AppHealth) ReportHealth(status *Status) {
	// If the user wants health probes only, short-circuit here
	if h.config.ProbeOnly {
		return
	}

	// Channel is buffered, so make sure that this doesn't block
	// Just in case another report is being worked on!
	select {
	case h.report <- status:
		// No action
	default:
		// No action
	}
}

// GetStatus returns the status of the app's health
func (h *AppHealth) GetStatus() *Status {
	fc := h.failureCount.Load()
	if fc >= h.config.Threshold {
		reason := fmt.Sprintf("App health check failed %d times", fc)
		return NewStatus(false, &reason)
	}

	return NewStatus(true, nil)
}

// Performs a health probe.
// Should be invoked in a background goroutine.
func (h *AppHealth) doProbe(parentCtx context.Context) {
	ctx, cancel := context.WithTimeout(parentCtx, h.config.ProbeTimeout)
	defer cancel()

	status, err := h.probeFn(ctx)
	if err != nil {
		reason := fmt.Sprintf("Probe error: %v", err)
		h.setResult(parentCtx, NewStatus(false, &reason))
		log.Errorf("App health probe could not complete with error: %v", err)
		return
	}

	// Only report if the status has changed
	currentStatus := h.GetStatus()
	if currentStatus.IsHealthy != status.IsHealthy {
		log.Debug("App health probe detected status change - health probe successful: " + strconv.FormatBool(status.IsHealthy))
		h.setResult(parentCtx, status)
	} else {
		log.Debug("App health probe status is unchanged - health probe successful: %v", strconv.FormatBool(status.IsHealthy))
	}
}

func (h *AppHealth) setResult(ctx context.Context, status *Status) {
	h.lastReport.Store(h.clock.Now().UnixMicro())

	if status.IsHealthy {
		// Reset the failure count
		// If the previous value was >= threshold, we need to report a health change
		prev := h.failureCount.Swap(0)
		if prev >= h.config.Threshold {
			log.Info("App entered healthy status")
			if h.changeCb != nil {
				h.wg.Add(1)
				go func() {
					defer h.wg.Done()
					h.changeCb(ctx, status)
				}()
			}
		}
		return
	}

	// Increment failure count atomically and get the new value
	newFailures := h.failureCount.Add(1)

	// Handle overflow
	if newFailures < 0 {
		newFailures = h.config.Threshold + 1
		h.failureCount.Store(newFailures)
	}

	// Notify when crossing threshold
	if newFailures == h.config.Threshold {
		if status.Reason != nil {
			log.Warn("App entered un-healthy status: " + *status.Reason)
		} else {
			log.Warn("App entered un-healthy status")
		}
		if h.changeCb != nil {
			h.wg.Add(1)
			go func() {
				defer h.wg.Done()
				h.changeCb(ctx, status)
			}()
		}
	}
}

func (h *AppHealth) Close() error {
	defer h.wg.Wait()
	if h.closed.CompareAndSwap(false, true) {
		close(h.closeCh)
	}

	return nil
}
