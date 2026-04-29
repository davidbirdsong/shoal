package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
)

// ServerState represents one server entry from HAProxy's runtime state.
type ServerState struct {
	Name    string
	Addr    string
	Port    uint32
	OpState string // "2" = UP, "0" = DOWN/MAINT
}

// haproxyClient abstracts HAProxy runtime socket operations.
type haproxyClient interface {
	AddServer(backend, key, addr string, port uint32) error
	RemoveServer(backend, key string) error
	ShowServers(backend string) ([]ServerState, error)
}

type haproxySocketClient struct {
	socketPath string
}

// query sends a command and returns the full response.
func (h *haproxySocketClient) query(cmd string) (string, error) {
	conn, err := net.Dial("unix", h.socketPath)
	if err != nil {
		return "", fmt.Errorf("haproxy: dial %s: %w", h.socketPath, err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "%s\n", cmd); err != nil {
		return "", err
	}
	var sb strings.Builder
	if _, err := bufio.NewReader(conn).WriteTo(&sb); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// send sends a command and discards the response.
func (h *haproxySocketClient) send(cmd string) error {
	_, err := h.query(cmd)
	return err
}

func (h *haproxySocketClient) AddServer(backend, key, addr string, port uint32) error {
	if err := h.send(fmt.Sprintf("add server %s/%s %s:%d", backend, key, addr, port)); err != nil {
		return err
	}
	return h.send(fmt.Sprintf("set server %s/%s state ready", backend, key))
}

func (h *haproxySocketClient) RemoveServer(backend, key string) error {
	return h.send(fmt.Sprintf("del server %s/%s", backend, key))
}

// ShowServers returns the current server list for the configured backend.
func (h *haproxySocketClient) ShowServers(backend string) ([]ServerState, error) {
	out, err := h.query(fmt.Sprintf("show servers state %s", backend))
	if err != nil {
		return nil, err
	}
	return parseServerStates(out), nil
}

// parseServerStates parses the text response from "show servers state <backend>".
// HAProxy response columns (1-indexed):
//
//	1=be_id 2=be_name 3=srv_id 4=srv_name 5=srv_addr 6=srv_op_state ... 12=srv_port
func parseServerStates(out string) []ServerState {
	var servers []ServerState
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 12 {
			continue
		}
		port64, _ := strconv.ParseUint(fields[11], 10, 32)
		servers = append(servers, ServerState{
			Name:    fields[3],
			Addr:    fields[4],
			Port:    uint32(port64),
			OpState: fields[5],
		})
	}
	return servers
}
