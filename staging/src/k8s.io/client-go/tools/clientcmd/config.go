/*
Copyright 2014 The Kubernetes Authors.

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

package clientcmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"k8s.io/klog/v2"

	restclient "k8s.io/client-go/rest"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// ConfigAccess is used by subcommands and methods in this package to load and modify the appropriate config files
type ConfigAccess interface {
	// GetLoadingPrecedence returns the slice of files that should be used for loading and inspecting the config
	GetLoadingPrecedence() []string
	// GetStartingConfig returns the config that subcommands should being operating against.  It may or may not be merged depending on loading rules
	GetStartingConfig() (*clientcmdapi.Config, error)
	// GetDefaultFilename returns the name of the file you should write into (create if necessary), if you're trying to create a new stanza as opposed to updating an existing one.
	GetDefaultFilename() string
	// IsExplicitFile indicates whether or not this command is interested in exactly one file.  This implementation only ever does that  via a flag, but implementations that handle local, global, and flags may have more
	IsExplicitFile() bool
	// GetExplicitFile returns the particular file this command is operating against.  This implementation only ever has one, but implementations that handle local, global, and flags may have more
	GetExplicitFile() string
}

type PathOptions struct {
	// GlobalFile is the full path to the file to load as the global (final) option
	GlobalFile string
	// EnvVar is the env var name that points to the list of kubeconfig files to load
	EnvVar string
	// ExplicitFileFlag is the name of the flag to use for prompting for the kubeconfig file
	ExplicitFileFlag string

	// GlobalFileSubpath is an optional value used for displaying help
	GlobalFileSubpath string

	LoadingRules *ClientConfigLoadingRules
}

var (
	// UseModifyConfigLock ensures that access to kubeconfig file using ModifyConfig method
	// is being guarded by a lock file.
	// This variable is intentionaly made public so other consumers of this library
	// can modify its default behavior, but be caution when disabling it since
	// this will make your code not threadsafe.
	UseModifyConfigLock = true
)

func (o *PathOptions) GetEnvVarFiles() []string {
	if len(o.EnvVar) == 0 {
		return []string{}
	}

	envVarValue := os.Getenv(o.EnvVar)
	if len(envVarValue) == 0 {
		return []string{}
	}

	fileList := filepath.SplitList(envVarValue)
	// prevent the same path load multiple times
	return deduplicate(fileList)
}

func (o *PathOptions) GetLoadingPrecedence() []string {
	if o.IsExplicitFile() {
		return []string{o.GetExplicitFile()}
	}

	if envVarFiles := o.GetEnvVarFiles(); len(envVarFiles) > 0 {
		return envVarFiles
	}
	return []string{o.GlobalFile}
}

func (o *PathOptions) GetStartingConfig() (*clientcmdapi.Config, error) {
	// don't mutate the original
	loadingRules := *o.LoadingRules
	loadingRules.Precedence = o.GetLoadingPrecedence()

	clientConfig := NewNonInteractiveDeferredLoadingClientConfig(&loadingRules, &ConfigOverrides{})
	rawConfig, err := clientConfig.RawConfig()
	if os.IsNotExist(err) {
		return clientcmdapi.NewConfig(), nil
	}
	if err != nil {
		return nil, err
	}

	return &rawConfig, nil
}

func (o *PathOptions) GetDefaultFilename() string {
	if o.IsExplicitFile() {
		return o.GetExplicitFile()
	}

	if envVarFiles := o.GetEnvVarFiles(); len(envVarFiles) > 0 {
		if len(envVarFiles) == 1 {
			return envVarFiles[0]
		}

		// if any of the envvar files already exists, return it
		for _, envVarFile := range envVarFiles {
			if _, err := os.Stat(envVarFile); err == nil {
				return envVarFile
			}
		}

		// otherwise, return the last one in the list
		return envVarFiles[len(envVarFiles)-1]
	}

	return o.GlobalFile
}

func (o *PathOptions) IsExplicitFile() bool {
	return len(o.LoadingRules.ExplicitPath) > 0
}

func (o *PathOptions) GetExplicitFile() string {
	return o.LoadingRules.ExplicitPath
}

func NewDefaultPathOptions() *PathOptions {
	ret := &PathOptions{
		GlobalFile:       RecommendedHomeFile,
		EnvVar:           RecommendedConfigPathEnvVar,
		ExplicitFileFlag: RecommendedConfigPathFlag,

		GlobalFileSubpath: filepath.Join(RecommendedHomeDir, RecommendedFileName),

		LoadingRules: NewDefaultClientConfigLoadingRules(),
	}
	ret.LoadingRules.DoNotResolvePaths = true

	return ret
}

// ModifyConfig takes a Config object, iterates through Clusters, AuthInfos, and Contexts, uses the LocationOfOrigin if specified or
// uses the default destination file to write the results into.  This results in multiple file reads, but it's very easy to follow.
// Preferences and CurrentContext should always be set in the default destination file.  Since we can't distinguish between empty and missing values
// (no nil strings), we're forced have separate handling for them.  In the kubeconfig cases, newConfig should have at most one difference,
// that means that this code will only write into a single file.  If you want to relativizePaths, you must provide a fully qualified path in any
// modified element.
func ModifyConfig(configAccess ConfigAccess, newConfig clientcmdapi.Config, relativizePaths bool) error {
	if UseModifyConfigLock {
		possibleSources := configAccess.GetLoadingPrecedence()
		// sort the possible kubeconfig files so we always "lock" in the same order
		// to avoid deadlock (note: this can fail w/ symlinks, but... come on).
		sort.Strings(possibleSources)
		for _, filename := range possibleSources {
			if err := lockFile(filename); err != nil {
				return err
			}
			defer unlockFile(filename)
		}
	}

	startingConfig, err := configAccess.GetStartingConfig()
	if err != nil {
		return err
	}

	// We need to find all differences, locate their original files, read a partial config to modify only that stanza and write out the file.
	// Special case the test for current context and preferences since those always write to the default file.
	if reflect.DeepEqual(*startingConfig, newConfig) {
		// nothing to do
		return nil
	}

	if startingConfig.CurrentContext != newConfig.CurrentContext {
		if err := writeCurrentContext(configAccess, newConfig.CurrentContext); err != nil {
			return err
		}
	}

	if !reflect.DeepEqual(startingConfig.Preferences, newConfig.Preferences) {
		if err := writePreferences(configAccess, newConfig.Preferences); err != nil {
			return err
		}
	}

	// Search every cluster, authInfo, and context.  First from new to old for differences, then from old to new for deletions
	for key, cluster := range newConfig.Clusters {
		startingCluster, exists := startingConfig.Clusters[key]
		if !reflect.DeepEqual(cluster, startingCluster) || !exists {
			destinationFile := cluster.LocationOfOrigin
			if len(destinationFile) == 0 {
				destinationFile = configAccess.GetDefaultFilename()
			}

			configToWrite, err := getConfigFromFile(destinationFile)
			if err != nil {
				return err
			}
			t := *cluster

			configToWrite.Clusters[key] = &t
			configToWrite.Clusters[key].LocationOfOrigin = destinationFile
			if relativizePaths {
				if err := RelativizeClusterLocalPaths(configToWrite.Clusters[key]); err != nil {
					return err
				}
			}

			if err := WriteToFile(*configToWrite, destinationFile); err != nil {
				return err
			}
		}
	}

	// seenConfigs stores a map of config source filenames to computed config objects
	seenConfigs := map[string]*clientcmdapi.Config{}

	for key, context := range newConfig.Contexts {
		startingContext, exists := startingConfig.Contexts[key]
		if !reflect.DeepEqual(context, startingContext) || !exists {
			destinationFile := context.LocationOfOrigin
			if len(destinationFile) == 0 {
				destinationFile = configAccess.GetDefaultFilename()
			}

			// we only obtain a fresh config object from its source file
			// if we have not seen it already - this prevents us from
			// reading and writing to the same number of files repeatedly
			// when multiple / all contexts share the same destination file.
			configToWrite, seen := seenConfigs[destinationFile]
			if !seen {
				var err error
				configToWrite, err = getConfigFromFile(destinationFile)
				if err != nil {
					return err
				}
				seenConfigs[destinationFile] = configToWrite
			}

			configToWrite.Contexts[key] = context
		}
	}

	// actually persist config object changes
	for destinationFile, configToWrite := range seenConfigs {
		if err := WriteToFile(*configToWrite, destinationFile); err != nil {
			return err
		}
	}

	for key, authInfo := range newConfig.AuthInfos {
		startingAuthInfo, exists := startingConfig.AuthInfos[key]
		if !reflect.DeepEqual(authInfo, startingAuthInfo) || !exists {
			destinationFile := authInfo.LocationOfOrigin
			if len(destinationFile) == 0 {
				destinationFile = configAccess.GetDefaultFilename()
			}

			configToWrite, err := getConfigFromFile(destinationFile)
			if err != nil {
				return err
			}
			t := *authInfo
			configToWrite.AuthInfos[key] = &t
			configToWrite.AuthInfos[key].LocationOfOrigin = destinationFile
			if relativizePaths {
				if err := RelativizeAuthInfoLocalPaths(configToWrite.AuthInfos[key]); err != nil {
					return err
				}
			}

			if err := WriteToFile(*configToWrite, destinationFile); err != nil {
				return err
			}
		}
	}

	for key, cluster := range startingConfig.Clusters {
		if _, exists := newConfig.Clusters[key]; !exists {
			destinationFile := cluster.LocationOfOrigin
			if len(destinationFile) == 0 {
				destinationFile = configAccess.GetDefaultFilename()
			}

			configToWrite, err := getConfigFromFile(destinationFile)
			if err != nil {
				return err
			}
			delete(configToWrite.Clusters, key)

			if err := WriteToFile(*configToWrite, destinationFile); err != nil {
				return err
			}
		}
	}

	for key, context := range startingConfig.Contexts {
		if _, exists := newConfig.Contexts[key]; !exists {
			destinationFile := context.LocationOfOrigin
			if len(destinationFile) == 0 {
				destinationFile = configAccess.GetDefaultFilename()
			}

			configToWrite, err := getConfigFromFile(destinationFile)
			if err != nil {
				return err
			}
			delete(configToWrite.Contexts, key)

			if err := WriteToFile(*configToWrite, destinationFile); err != nil {
				return err
			}
		}
	}

	for key, authInfo := range startingConfig.AuthInfos {
		if _, exists := newConfig.AuthInfos[key]; !exists {
			destinationFile := authInfo.LocationOfOrigin
			if len(destinationFile) == 0 {
				destinationFile = configAccess.GetDefaultFilename()
			}

			configToWrite, err := getConfigFromFile(destinationFile)
			if err != nil {
				return err
			}
			delete(configToWrite.AuthInfos, key)

			if err := WriteToFile(*configToWrite, destinationFile); err != nil {
				return err
			}
		}
	}

	return nil
}

func PersisterForUser(configAccess ConfigAccess, user string) restclient.AuthProviderConfigPersister {
	return &persister{configAccess, user}
}

type persister struct {
	configAccess ConfigAccess
	user         string
}

func (p *persister) Persist(config map[string]string) error {
	newConfig, err := p.configAccess.GetStartingConfig()
	if err != nil {
		return err
	}
	authInfo, ok := newConfig.AuthInfos[p.user]
	if ok && authInfo.AuthProvider != nil {
		authInfo.AuthProvider.Config = config
		return ModifyConfig(p.configAccess, *newConfig, false)
	}
	return nil
}

// writeCurrentContext takes three possible paths.
// If newCurrentContext is the same as the startingConfig's current context, then we exit.
// If newCurrentContext has a value, then that value is written into the default destination file.
// If newCurrentContext is empty, then we find the config file that is setting the CurrentContext and clear the value from that file
func writeCurrentContext(configAccess ConfigAccess, newCurrentContext string) error {
	if startingConfig, err := configAccess.GetStartingConfig(); err != nil {
		return err
	} else if startingConfig.CurrentContext == newCurrentContext {
		return nil
	}

	if configAccess.IsExplicitFile() {
		file := configAccess.GetExplicitFile()
		currConfig, err := getConfigFromFile(file)
		if err != nil {
			return err
		}
		currConfig.CurrentContext = newCurrentContext
		if err := WriteToFile(*currConfig, file); err != nil {
			return err
		}

		return nil
	}

	if len(newCurrentContext) > 0 {
		destinationFile := configAccess.GetDefaultFilename()
		config, err := getConfigFromFile(destinationFile)
		if err != nil {
			return err
		}
		config.CurrentContext = newCurrentContext

		if err := WriteToFile(*config, destinationFile); err != nil {
			return err
		}

		return nil
	}

	// we're supposed to be clearing the current context.  We need to find the first spot in the chain that is setting it and clear it
	for _, file := range configAccess.GetLoadingPrecedence() {
		if _, err := os.Stat(file); err == nil {
			currConfig, err := getConfigFromFile(file)
			if err != nil {
				return err
			}

			if len(currConfig.CurrentContext) > 0 {
				currConfig.CurrentContext = newCurrentContext
				if err := WriteToFile(*currConfig, file); err != nil {
					return err
				}

				return nil
			}
		}
	}

	return errors.New("no config found to write context")
}

func writePreferences(configAccess ConfigAccess, newPrefs clientcmdapi.Preferences) error {
	if startingConfig, err := configAccess.GetStartingConfig(); err != nil {
		return err
	} else if reflect.DeepEqual(startingConfig.Preferences, newPrefs) {
		return nil
	}

	if configAccess.IsExplicitFile() {
		file := configAccess.GetExplicitFile()
		currConfig, err := getConfigFromFile(file)
		if err != nil {
			return err
		}
		currConfig.Preferences = newPrefs
		if err := WriteToFile(*currConfig, file); err != nil {
			return err
		}

		return nil
	}

	for _, file := range configAccess.GetLoadingPrecedence() {
		currConfig, err := getConfigFromFile(file)
		if err != nil {
			return err
		}

		if !reflect.DeepEqual(currConfig.Preferences, newPrefs) {
			currConfig.Preferences = newPrefs
			if err := WriteToFile(*currConfig, file); err != nil {
				return err
			}

			return nil
		}
	}

	return errors.New("no config found to write preferences")
}

// getConfigFromFile tries to read a kubeconfig file and if it can't, returns an error.  One exception, missing files result in empty configs, not an error.
func getConfigFromFile(filename string) (*clientcmdapi.Config, error) {
	config, err := LoadFromFile(filename)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if config == nil {
		config = clientcmdapi.NewConfig()
	}
	return config, nil
}

// GetConfigFromFileOrDie tries to read a kubeconfig file and if it can't, it calls exit.  One exception, missing files result in empty configs, not an exit
func GetConfigFromFileOrDie(filename string) *clientcmdapi.Config {
	config, err := getConfigFromFile(filename)
	if err != nil {
		//nolint:logcheck // A helper function like this should not log. But this is probably part of the the established client-go API and not worth changing.
		klog.FatalDepth(1, err)
	}

	return config
}
