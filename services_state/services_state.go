package services_state

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hashicorp/memberlist"
	"github.com/newrelic/bosun/service"
)

const (
	TOMBSTONE_LIFESPAN = 3 * time.Hour // How long we keep tombstones around
	TOMBSTONE_COUNT = 10               // Send 1/second 10 times
)

// Holds the state about one server in our cluster
type Server struct {
	Name string
	Services map[string]*service.Service
	LastUpdated time.Time
}

func NewServer(name string) *Server {
	var server Server
	server.Name = name
	// Pre-create for 5 services per host
	server.Services = make(map[string]*service.Service, 5)
	server.LastUpdated = time.Unix(0, 0)

	return &server
}

// Holds the state about all the servers in the cluster
type ServicesState struct {
	Servers map[string]*Server
}

func NewServicesState() *ServicesState {
	var state ServicesState
	state.Servers = make(map[string]*Server, 5)
	return &state
}

func (state *ServicesState) Encode() []byte {
	jsonData, err := json.Marshal(state.Servers)
	if err != nil {
		log.Println("ERROR: Failed to Marshal state")
		return []byte{}
	}

	return jsonData
}

func (state *ServicesState) HasServer(hostname string) bool {
	if state.Servers[hostname] != nil {
		return true
	}

	return false
}

func (state *ServicesState) AddServiceEntry(entry service.Service) {

	if !state.HasServer(entry.Hostname) {
		state.Servers[entry.Hostname] = NewServer(entry.Hostname)
	}

	containerRef := state.Servers[entry.Hostname]
	// Only apply changes that are newer
	if containerRef.Services[entry.ID] == nil ||
			entry.Invalidates(containerRef.Services[entry.ID]) {
		containerRef.Services[entry.ID] = &entry
	}

	containerRef.LastUpdated = time.Now().UTC()
}

func (state *ServicesState) Merge(otherState *ServicesState) {
	for _, server := range otherState.Servers {
		for _, service := range server.Services {
			state.AddServiceEntry(*service)
		}
	}
}

func (state *ServicesState) Format(list *memberlist.Memberlist) string {
	var output string

	output += "Services ------------------------------\n"
	for hostname, server := range state.Servers {
		output += fmt.Sprintf("  %s: (%s)\n", hostname, server.LastUpdated.String())
		for _, service := range server.Services {
			output += service.Format()
		}
		output += "\n"
	}

	// Don't show member list
	if list == nil {
		return output
	}

	output += "\nCluster Hosts -------------------------\n"
	for _, host := range list.Members() {
		output += fmt.Sprintf("    %s\n", host.Name)
	}

	output += "---------------------------------------"

	return output
}

func (state *ServicesState) Print(list *memberlist.Memberlist) {
	log.Println(state.Format(list))
}

// Loops forever, keeping our state current, and transmitting state
// on the broadcast channel. Intended to run as a background goroutine.
func (state *ServicesState) StayCurrent(broadcasts chan [][]byte, fn func() []service.Service ) {
	for ;; {
		containerList := fn()
		prepared := make([][]byte, len(containerList))

		for _, container := range containerList {
			state.AddServiceEntry(container)
			encoded, err := container.Encode()
			if err != nil {
				log.Printf("ERROR encoding container: (%s)", err.Error())
				continue
			}

			prepared = append(prepared, encoded)
		}

		// Tell people about our dead services
		tombstones := state.TombstoneServices(containerList)

		if tombstones != nil {
			// Announce them now
			prepared = append(prepared, tombstones...)

			// Announce these every second for awhile
			go func() {
				for i := 0; i < TOMBSTONE_COUNT; i++ {
					broadcasts <- tombstones
					time.Sleep(1 * time.Second)
				}
			}()

		}

		broadcasts <- prepared // Put it on the wire

		time.Sleep(2 * time.Second)
	}
}

func (state *ServicesState) TombstoneServices(containerList []service.Service) [][]byte {
	hostname, _ := os.Hostname()

	if !state.HasServer(hostname) {
		log.Println("TombstoneServices(): New host or not running services, skipping.")
		return nil
	}

	result := make([][]byte, len(containerList))

	// Build a map from the list first
	mapping := makeServiceMapping(containerList)

	// Copy this so we can change the real list in the loop
	services := state.Servers[hostname].Services

	for id, svc := range services {
		// Manage tombstone life so we don't keep them forever
		if svc.Status == service.TOMBSTONE &&
				svc.Updated.Before(time.Now().UTC().Add(0 - TOMBSTONE_LIFESPAN)) {
			delete(state.Servers[hostname].Services, id)
			delete(mapping, id)
		}

		if mapping[id] == nil && svc.Status == service.ALIVE {
			log.Printf("Tombstoning %s\n", svc.ID)
			svc.Tombstone()
			// Tombstone each record twice to help with receipt
			// TODO do we need to do this now that we send them 10 times?
			for i := 0; i < 2; i++ {
				encoded, err := svc.Encode()
				if err != nil {
					log.Printf("ERROR encoding container: (%s)", err.Error())
					continue
				}

				result = append(result, encoded)
			}
		}
	}

	return result
}

func makeServiceMapping(containerList []service.Service) map[string]*service.Service {
	mapping := make(map[string]*service.Service, len(containerList))
	for _, container := range containerList {
		mapping[container.ID] = &container
	}

	return mapping
}

func Decode(data []byte) (*ServicesState, error) {
	newState := NewServicesState()
	err := json.Unmarshal(data, &newState.Servers)
	if err != nil {
		log.Printf("Error decoding state! (%s)", err.Error())
	}

	return newState, err
}