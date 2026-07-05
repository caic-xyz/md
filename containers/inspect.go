// Copyright 2026 Marc-Antoine Ruel. All Rights Reserved. Use of this
// source code is governed by the Apache v2 license that can be found in the
// LICENSE file.

package containers

import (
	"encoding/json"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"time"
)

// Container describes normalized Docker/Podman container metadata.
type Container struct {
	Name      string
	State     string
	CreatedAt time.Time
	Labels    map[string]string
	SSHPort   int32
	VNCPort   int32
}

// Mount describes an observed runtime mount.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// InspectInfo describes observed Docker/Podman runtime configuration for a container.
type InspectInfo struct {
	Runtime      string
	ID           string
	Name         string
	State        string
	ImageRef     string
	ImageID      string
	Platform     string
	OS           string
	Architecture string
	CPULimit     int
	Mounts       []Mount
	Labels       map[string]string
}

// parseContainerListLine parses one Docker/Podman ps JSON line.
func parseContainerListLine(data []byte) (Container, error) {
	var raw containerJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return Container{}, err
	}
	ct := Container{
		Name:   string(raw.Names),
		State:  raw.State,
		Labels: maps.Clone(map[string]string(raw.Labels)),
	}
	if raw.CreatedAt != "" {
		t, err := parseCreatedAt(raw.CreatedAt)
		if err != nil {
			return Container{}, err
		}
		ct.CreatedAt = t
	}
	fillPorts(&ct, string(raw.Ports))
	return ct, nil
}

// ParseInspectContainer parses Docker/Podman inspect JSON into container metadata.
func ParseInspectContainer(data []byte) (*Container, error) {
	raw, err := inspectDocument(data)
	if err != nil {
		return nil, err
	}
	ct := &Container{
		Name:    strings.TrimPrefix(raw.Name, "/"),
		State:   raw.State.Status,
		Labels:  maps.Clone(raw.Config.Labels),
		SSHPort: hostPort(raw.NetworkSettings.Ports, "22/tcp"),
		VNCPort: hostPort(raw.NetworkSettings.Ports, "5901/tcp"),
	}
	if t, err := parseCreatedAt(raw.Created); err == nil {
		ct.CreatedAt = t
	}
	return ct, nil
}

// parseInspectInfo parses Docker/Podman inspect JSON into InspectInfo.
func parseInspectInfo(runtimeName, requestedName string, data []byte) (*InspectInfo, error) {
	raw, err := inspectDocument(data)
	if err != nil {
		return nil, err
	}
	mounts := make([]Mount, 0, len(raw.Mounts))
	for _, m := range raw.Mounts {
		if m.Source == "" && m.Destination == "" {
			continue
		}
		readOnly := false
		if m.RW != nil {
			readOnly = !*m.RW
		}
		mounts = append(mounts, Mount{HostPath: m.Source, ContainerPath: m.Destination, ReadOnly: readOnly})
	}
	name := strings.TrimPrefix(raw.Name, "/")
	if name == "" {
		name = requestedName
	}
	osName, architecture := inspectOSArch(&raw)
	return &InspectInfo{
		Runtime:      runtimeName,
		ID:           raw.ID,
		Name:         name,
		State:        raw.State.Status,
		ImageRef:     raw.Config.Image,
		ImageID:      raw.Image,
		Platform:     inspectPlatform(&raw),
		OS:           osName,
		Architecture: architecture,
		CPULimit:     inspectCPULimit(raw.HostConfig),
		Mounts:       mounts,
		Labels:       maps.Clone(raw.Config.Labels),
	}, nil
}

// parseCreatedAt parses a container creation timestamp.
func parseCreatedAt(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05 -0700 MST",
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse CreatedAt %q", s)
}

func fillPorts(ct *Container, ports string) {
	for mapping := range strings.SplitSeq(ports, ",") {
		mapping = strings.TrimSpace(mapping)
		if mapping == "" {
			continue
		}
		hostPart, containerPart, ok := strings.Cut(mapping, "->")
		if !ok {
			continue
		}
		containerPortStr, _, _ := strings.Cut(containerPart, "/")
		hostPortStr := hostPart
		if idx := strings.LastIndex(hostPart, ":"); idx >= 0 {
			hostPortStr = hostPart[idx+1:]
		}
		hostPort, err := strconv.ParseInt(hostPortStr, 10, 32)
		if err != nil {
			continue
		}
		containerPort, err := strconv.ParseInt(containerPortStr, 10, 32)
		if err != nil {
			continue
		}
		switch int32(containerPort) {
		case 22:
			ct.SSHPort = int32(hostPort)
		case 5901:
			ct.VNCPort = int32(hostPort)
		}
	}
}

// psName handles Docker's string format and Podman's array format for Names.
type psName string

func (n *psName) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '[' {
		var names []string
		if err := json.Unmarshal(data, &names); err != nil {
			return err
		}
		if len(names) > 0 {
			*n = psName(names[0])
		}
		return nil
	}
	return json.Unmarshal(data, (*string)(n))
}

// psPorts handles Docker's string format and Podman's array format for Ports.
type psPorts string

type podmanPortMapping struct {
	HostIP        string `json:"host_ip"`
	HostPort      uint16 `json:"host_port"`
	ContainerPort uint16 `json:"container_port"`
	Proto         string `json:"proto"`
}

func (p *psPorts) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '[' {
		var mappings []podmanPortMapping
		if err := json.Unmarshal(data, &mappings); err != nil {
			return err
		}
		parts := make([]string, 0, len(mappings))
		for _, m := range mappings {
			host := m.HostIP
			if host == "" {
				host = "0.0.0.0"
			}
			parts = append(parts, fmt.Sprintf("%s:%d->%d/%s", host, m.HostPort, m.ContainerPort, m.Proto))
		}
		*p = psPorts(strings.Join(parts, ", "))
		return nil
	}
	return json.Unmarshal(data, (*string)(p))
}

// psLabels handles Docker's comma-separated string format and Podman's JSON object format.
type psLabels map[string]string

func (l *psLabels) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	*l = make(map[string]string)
	if data[0] == '{' {
		return json.Unmarshal(data, (*map[string]string)(l))
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	for kv := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		(*l)[k] = v
	}
	return nil
}

type containerJSON struct {
	Names     psName   `json:"Names"`
	State     string   `json:"State"`
	CreatedAt string   `json:"CreatedAt"`
	Labels    psLabels `json:"Labels"`
	Ports     psPorts  `json:"Ports"`
}

type containerInspectJSON struct {
	ID              string             `json:"Id"`
	Name            string             `json:"Name"`
	Image           string             `json:"Image"`
	Platform        string             `json:"Platform"`
	Architecture    string             `json:"Architecture"`
	OS              string             `json:"Os"`
	State           inspectStateJSON   `json:"State"`
	Created         string             `json:"Created"`
	Config          inspectConfigJSON  `json:"Config"`
	HostConfig      inspectHostJSON    `json:"HostConfig"`
	NetworkSettings inspectNetworkJSON `json:"NetworkSettings"`
	Mounts          []inspectMountJSON `json:"Mounts"`
}

type inspectConfigJSON struct {
	Image  string            `json:"Image"`
	Labels map[string]string `json:"Labels"`
}

type inspectHostJSON struct {
	NanoCpus  int64 `json:"NanoCpus"`
	CPUQuota  int64 `json:"CpuQuota"`
	CPUPeriod int64 `json:"CpuPeriod"`
}

type inspectNetworkJSON struct {
	Ports map[string][]inspectPortBindingJSON `json:"Ports"`
}

type inspectPortBindingJSON struct {
	HostPort string `json:"HostPort"`
}

type inspectMountJSON struct {
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          *bool  `json:"RW"`
}

type inspectStateJSON struct {
	Status string `json:"Status"`
}

func inspectDocument(data []byte) (containerInspectJSON, error) {
	var raws []containerInspectJSON
	if err := json.Unmarshal(data, &raws); err != nil {
		return containerInspectJSON{}, err
	}
	if len(raws) != 1 {
		return containerInspectJSON{}, fmt.Errorf("inspect returned %d results, expected 1", len(raws))
	}
	return raws[0], nil
}

func inspectPlatform(raw *containerInspectJSON) string {
	if raw.Platform != "" {
		return raw.Platform
	}
	osName, architecture := inspectOSArch(raw)
	if osName != "" && architecture != "" {
		return osName + "/" + architecture
	}
	return ""
}

func inspectOSArch(raw *containerInspectJSON) (osName, architecture string) {
	if osName, architecture, ok := splitOSArch(raw.Platform, ""); ok {
		return osName, architecture
	}
	osName = cleanInspectValue(raw.OS)
	if osName == "" {
		osName = cleanInspectValue(raw.Platform)
	}
	architecture = cleanInspectValue(raw.Architecture)
	return osName, architecture
}

func splitOSArch(platform, fallbackOS string) (osName, architecture string, ok bool) {
	osName, architecture, ok = strings.Cut(platform, "/")
	if !ok {
		return "", "", false
	}
	osName = cleanInspectValue(osName)
	if osName == "" {
		osName = fallbackOS
	}
	architecture = cleanInspectValue(architecture)
	if osName == "" || architecture == "" {
		return "", "", false
	}
	return osName, architecture, true
}

func cleanInspectValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "<no value>" {
		return ""
	}
	return v
}

func inspectCPULimit(c inspectHostJSON) int {
	if c.NanoCpus > 0 {
		return int((c.NanoCpus + 1_000_000_000 - 1) / 1_000_000_000)
	}
	if c.CPUQuota > 0 && c.CPUPeriod > 0 {
		return int((c.CPUQuota + c.CPUPeriod - 1) / c.CPUPeriod)
	}
	return 0
}

func hostPort(ports map[string][]inspectPortBindingJSON, containerPort string) int32 {
	bindings := ports[containerPort]
	if len(bindings) == 0 {
		return 0
	}
	port, err := strconv.ParseInt(bindings[0].HostPort, 10, 32)
	if err != nil {
		return 0
	}
	return int32(port)
}
