package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"reflect"

	"github.com/yokecd/yoke/pkg/apis/v1alpha1"
	"github.com/yokecd/yoke/pkg/openapi"

	agentv1alpha1 "github.com/tigrisdata-community/mithras/internal/k8s/agent/v1alpha1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	flightURL = flag.String("flight-url", "oci://ghcr.io/tigrisdata-community/mithras/crd/agent/flight:v1alpha1", "the URL to the Wasm module to load")
)

func main() {
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return json.NewEncoder(os.Stdout).Encode(v1alpha1.Airway{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mithras.tigris.sh-v1alpha1-agent",
		},
		Spec: v1alpha1.AirwaySpec{
			ClusterAccess: true,
			WasmURLs: v1alpha1.WasmURLs{
				Flight: *flightURL,
			},
			Template: apiextv1.CustomResourceDefinitionSpec{
				Group: "mithras.tigris.sh",
				Names: apiextv1.CustomResourceDefinitionNames{
					Plural:   "agents",
					Singular: "agent",
					Kind:     "Agent",
				},
				Scope: apiextv1.NamespaceScoped,
				Versions: []apiextv1.CustomResourceDefinitionVersion{
					{
						Name:    "v1alpha1",
						Served:  true,
						Storage: true,
						Schema: &apiextv1.CustomResourceValidation{
							OpenAPIV3Schema: openapi.SchemaFrom(reflect.TypeFor[agentv1alpha1.Agent]()),
						},
					},
				},
			},
		},
	})
}
