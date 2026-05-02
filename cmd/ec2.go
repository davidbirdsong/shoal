package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	imdsTokenURL = "http://169.254.169.254/latest/api/token"
	imdsIPURL    = "http://169.254.169.254/latest/meta-data/local-ipv4"
	imdsTimeout  = 2 * time.Second
)

// ec2PrivateIP returns the instance's private IPv4 address via IMDSv2.
// Returns an error if the host is not an EC2 instance or the metadata
// service is unreachable.
func ec2PrivateIP(ctx context.Context) (string, error) {
	client := &http.Client{Timeout: imdsTimeout}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, imdsTokenURL, nil)
	if err != nil {
		return "", fmt.Errorf("ec2 metadata: build token request: %w", err)
	}
	req.Header.Set("X-aws-ec2-metadata-token-ttl-seconds", "21600")
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ec2 metadata: token request: %w", err)
	}
	defer resp.Body.Close()
	token, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ec2 metadata: read token: %w", err)
	}

	req, err = http.NewRequestWithContext(ctx, http.MethodGet, imdsIPURL, nil)
	if err != nil {
		return "", fmt.Errorf("ec2 metadata: build ip request: %w", err)
	}
	req.Header.Set("X-aws-ec2-metadata-token", strings.TrimSpace(string(token)))
	resp, err = client.Do(req)
	if err != nil {
		return "", fmt.Errorf("ec2 metadata: ip request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ec2 metadata: unexpected status %d", resp.StatusCode)
	}
	ip, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("ec2 metadata: read ip: %w", err)
	}
	return strings.TrimSpace(string(ip)), nil
}
