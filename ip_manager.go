package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

type IPManager struct {
	*IPConfiguration

	states       <-chan bool
	currentState bool
	stateLock    sync.Mutex
	recheck      *sync.Cond
}

func NewIPManager(config *IPConfiguration, states <-chan bool) *IPManager {
	m := &IPManager{
		IPConfiguration: config,
		states:          states,
		currentState:    false,
	}

	m.recheck = sync.NewCond(&m.stateLock)

	return m
}

func (m *IPManager) applyLoop(ctx context.Context) {
	for {
		actualState := m.QueryAddress()
		m.stateLock.Lock()
		desiredState := m.currentState
		log.Printf("IP address %s state is %t, desired %t", m.GetCIDR(), actualState, desiredState)
		if actualState != desiredState {
			m.stateLock.Unlock()
			if desiredState {
				m.ConfigureAddress()
			} else {
				m.DeconfigureAddress()
			}
		} else {
			// Wait for notification
			m.recheck.Wait()
			// Want to query actual state anyway, so unlock
			m.stateLock.Unlock()

			// Check if we should exit
			select {
			case <-ctx.Done():
				m.DeconfigureAddress()
				return
			default:
			}
		}
	}
}

func (m *IPManager) SyncStates(ctx context.Context, states <-chan bool) {
	ticker := time.NewTicker(10 * time.Second)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		m.applyLoop(ctx)
		wg.Done()
	}()

	for {
		select {
		case newState := <-states:
			m.stateLock.Lock()
			if m.currentState != newState {
				m.currentState = newState
				m.recheck.Broadcast()
			}
			m.stateLock.Unlock()
		case <-ticker.C:
			m.recheck.Broadcast()
		case <-ctx.Done():
			m.recheck.Broadcast()
			wg.Wait()
			return
		}
	}
}

func (m *IPManager) ARPQueryDuplicates() bool {
	c := exec.Command("arping",
		"-D", "-c", "2", "-q", "-w", "3",
		"-I", m.iface, m.vip.String())
	err := c.Run()
	if err != nil {
		return false
	}
	return true
}

func (m *IPManager) QueryAddress() bool {
	c := exec.Command("ip", "addr", "show", m.iface)

	lookup := fmt.Sprintf("inet %s", m.GetCIDR())
	result := false

	stdout, err := c.StdoutPipe()
	if err != nil {
		panic(err)
	}

	err = c.Start()
	if err != nil {
		panic(err)
	}

	scn := bufio.NewScanner(stdout)

	for scn.Scan() {
		line := scn.Text()
		if strings.Contains(line, lookup) {
			result = true
		}
	}

	c.Wait()

	return result
}

func (m *IPManager) ConfigureAddress() bool {
	log.Printf("Configuring address %s on %s", m.GetCIDR(), m.iface)
	return m.runAddressConfiguration("add")
}

func (m *IPManager) DeconfigureAddress() bool {
	log.Printf("Removing address %s on %s", m.GetCIDR(), m.iface)
	return m.runAddressConfiguration("delete")
}

func (m *IPManager) runAddressConfiguration(action string) bool {
	c := exec.Command("ip", "addr", action,
		m.GetCIDR(),
		"dev", m.iface)
	err := c.Run()

	switch exit := err.(type) {
	case *exec.ExitError:
		if status, ok := exit.Sys().(syscall.WaitStatus); ok {
			if status.ExitStatus() == 2 {
				// Already exists
				return true
			} else {
				log.Printf("Got error %s", status)
			}
		}

		return false
	}
	if err != nil {
		log.Printf("Error running ip address %s %s on %s: %s",
			action, m.vip, m.iface, err)
		return false
	}
	return true
}