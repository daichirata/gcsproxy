package main

import (
	"context"
	"io/ioutil"
	"log"

	secretmanager "cloud.google.com/go/secretmanager/apiv1"
	secretmanagerpb "google.golang.org/genproto/googleapis/cloud/secretmanager/v1"
)

// GetSecret from google secret manager. Format: "projects/{project numerical id}/secrets/{key name}/versions/{key version}"
func GetSecret(s string) string {
	// Create the client.
	ctx := context.Background()
	client, err := secretmanager.NewClient(ctx)
	if err != nil {
		log.Fatalf("failed to setup client: %v", err)
	}

	// Build the request.
	accessRequest := &secretmanagerpb.AccessSecretVersionRequest{
		Name: s,
	}
	// Call the API.
	result, err := client.AccessSecretVersion(ctx, accessRequest)
	if err != nil {
		log.Fatalf("failed to access secret version: %v", err)
	}

	if err := ioutil.WriteFile("/tmp/sa.json", result.Payload.Data, 0600); err != nil {
		log.Fatalf("failed to write sa json key: %v", err)
	}

	return "/tmp/sa.json"
}
