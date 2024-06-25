// Code generated by go-swagger; DO NOT EDIT.

// Copyright Authors of Cilium
// SPDX-License-Identifier: Apache-2.0

package server

// This file was generated by the swagger tool.
// Editing this file might prove futile when you re-run the swagger generate command

import (
	"encoding/json"
)

var (
	// SwaggerJSON embedded version of the swagger document used at generation time
	SwaggerJSON json.RawMessage
	// FlatSwaggerJSON embedded flattened version of the swagger document used at generation time
	FlatSwaggerJSON json.RawMessage
)

func init() {
	SwaggerJSON = json.RawMessage([]byte(`{
  "consumes": [
    "application/json"
  ],
  "produces": [
    "application/json"
  ],
  "swagger": "2.0",
  "info": {
    "description": "Cilium KVStoreMesh",
    "title": "KvstoreMesh",
    "version": "v1beta1"
  },
  "basePath": "/v1",
  "paths": {
    "/cluster": {
      "get": {
        "tags": [
          "cluster"
        ],
        "summary": "Retrieve the list of remote clusters and their status",
        "responses": {
          "200": {
            "description": "Success",
            "schema": {
              "type": "array",
              "items": {
                "$ref": "#/definitions/RemoteCluster"
              }
            }
          }
        }
      }
    }
  },
  "definitions": {
    "RemoteCluster": {
      "allOf": [
        {
          "$ref": "../openapi.yaml#/definitions/RemoteCluster"
        }
      ],
      "x-go-type": {
        "import": {
          "alias": "common",
          "package": "github.com/cilium/cilium/api/v1/models"
        },
        "type": "RemoteCluster"
      }
    }
  },
  "x-schemes": [
    "unix"
  ]
}`))
	FlatSwaggerJSON = json.RawMessage([]byte(`{
  "consumes": [
    "application/json"
  ],
  "produces": [
    "application/json"
  ],
  "swagger": "2.0",
  "info": {
    "description": "Cilium KVStoreMesh",
    "title": "KvstoreMesh",
    "version": "v1beta1"
  },
  "basePath": "/v1",
  "paths": {
    "/cluster": {
      "get": {
        "tags": [
          "cluster"
        ],
        "summary": "Retrieve the list of remote clusters and their status",
        "responses": {
          "200": {
            "description": "Success",
            "schema": {
              "type": "array",
              "items": {
                "$ref": "#/definitions/RemoteCluster"
              }
            }
          }
        }
      }
    }
  },
  "definitions": {
    "RemoteCluster": {
      "allOf": [
        {
          "description": "Status of remote cluster\n\n+k8s:deepcopy-gen=true",
          "properties": {
            "config": {
              "$ref": "#/definitions/remoteClusterConfig"
            },
            "connected": {
              "description": "Indicates whether the connection to the remote kvstore is established",
              "type": "boolean"
            },
            "last-failure": {
              "description": "Time of last failure that occurred while attempting to reach the cluster",
              "type": "string",
              "format": "date-time"
            },
            "name": {
              "description": "Name of the cluster",
              "type": "string"
            },
            "num-endpoints": {
              "description": "Number of endpoints in the cluster",
              "type": "integer"
            },
            "num-failures": {
              "description": "Number of failures reaching the cluster",
              "type": "integer"
            },
            "num-identities": {
              "description": "Number of identities in the cluster",
              "type": "integer"
            },
            "num-nodes": {
              "description": "Number of nodes in the cluster",
              "type": "integer"
            },
            "num-service-exports": {
              "description": "Number of MCS-API service exports in the cluster",
              "type": "integer"
            },
            "num-shared-services": {
              "description": "Number of services in the cluster",
              "type": "integer"
            },
            "ready": {
              "description": "Indicates readiness of the remote cluster",
              "type": "boolean"
            },
            "status": {
              "description": "Status of the control plane",
              "type": "string"
            },
            "synced": {
              "$ref": "#/definitions/remoteClusterSynced"
            }
          }
        }
      ],
      "x-go-type": {
        "import": {
          "alias": "common",
          "package": "github.com/cilium/cilium/api/v1/models"
        },
        "type": "RemoteCluster"
      }
    },
    "remoteClusterConfig": {
      "description": "Cluster configuration exposed by the remote cluster\n\n+k8s:deepcopy-gen=true",
      "properties": {
        "cluster-id": {
          "description": "The Cluster ID advertised by the remote cluster",
          "type": "integer"
        },
        "kvstoremesh": {
          "description": "Whether the remote cluster information is locally cached by kvstoremesh",
          "type": "boolean"
        },
        "required": {
          "description": "Whether the configuration is required to be present",
          "type": "boolean"
        },
        "retrieved": {
          "description": "Whether the configuration has been correctly retrieved",
          "type": "boolean"
        },
        "service-exports-enabled": {
          "description": "Whether or not MCS-API ServiceExports is enabled by the cluster.",
          "type": "boolean",
          "x-nullable": true
        },
        "sync-canaries": {
          "description": "Whether the remote cluster supports per-prefix \"synced\" canaries",
          "type": "boolean"
        }
      }
    },
    "remoteClusterSynced": {
      "description": "Status of the synchronization with the remote cluster, about each resource\ntype. A given resource is considered to be synchronized if the initial\nlist of entries has been completely received from the remote cluster, and\nnew events are currently being watched.\n\n+k8s:deepcopy-gen=true",
      "properties": {
        "endpoints": {
          "description": "Endpoints synchronization status",
          "type": "boolean"
        },
        "identities": {
          "description": "Identities synchronization status",
          "type": "boolean"
        },
        "nodes": {
          "description": "Nodes synchronization status",
          "type": "boolean"
        },
        "service-exports": {
          "description": "MCS-API service exports synchronization status",
          "type": "boolean"
        },
        "services": {
          "description": "Services synchronization status",
          "type": "boolean"
        }
      }
    }
  },
  "x-schemes": [
    "unix"
  ]
}`))
}
