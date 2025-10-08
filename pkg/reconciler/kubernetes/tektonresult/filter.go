/*
Copyright 2023 The Tekton Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" B]>SIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package tektonresult

import (
	mf "github.com/manifestival/manifestival"
	"github.com/tektoncd/operator/pkg/apis/operator/v1alpha1"
)

const (
	statefulSetDB     = "tekton-results-postgres"
	servicePostgresDB = "tekton-results-postgres-service"
	watcherDeployment = "tekton-results-watcher"
	watcherService    = "tekton-results-watcher"
	watcherConfigMap  = "tekton-results-config-leader-election"
)

func filterExternalDB(tr *v1alpha1.TektonResult, manifest *mf.Manifest) {
	if tr.Spec.IsExternalDB {
		*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("StatefulSet"), mf.ByName(statefulSetDB))))
		*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("ConfigMap"), mf.ByName(configPostgresDB))))
		*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("Service"), mf.ByName(servicePostgresDB))))
	}
}

// filterWatcherAndRetentionPolicy filters out watcher deployment and retention policy components
// This allows deploying only the Results API server without the watcher and retention policy agent
func filterWatcherAndRetentionPolicy(manifest *mf.Manifest) {
	// Remove watcher deployment
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("Deployment"), mf.ByName(watcherDeployment))))

	// Remove watcher service
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("Service"), mf.ByName(watcherService))))

	// Remove watcher-related ConfigMaps (leader election config is used by watcher)
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("ConfigMap"), mf.ByName(watcherConfigMap))))

	// Remove watcher ServiceAccount if it exists as a separate resource
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("ServiceAccount"), mf.ByName(watcherDeployment))))

	// Remove watcher-specific RBAC resources
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("ClusterRole"), mf.ByName(watcherDeployment))))
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("ClusterRoleBinding"), mf.ByName(watcherDeployment))))
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("Role"), mf.ByName(watcherDeployment))))
	*manifest = manifest.Filter(mf.Not(mf.All(mf.ByKind("RoleBinding"), mf.ByName(watcherDeployment))))
}
