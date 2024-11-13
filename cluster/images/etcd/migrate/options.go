/*
Copyright 2018 The Kubernetes Authors.

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

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	flag "github.com/spf13/pflag"
	"k8s.io/klog/v2"
)

var (
	supportedEtcdVersions = []string{"3.4.18", "3.5.17"}
)

const (
	etcdNameEnv           = "ETCD_NAME"
	etcdHostnameEnv       = "ETCD_HOSTNAME"
	hostnameEnv           = "HOSTNAME"
	dataDirEnv            = "DATA_DIRECTORY"
	initialClusterEnv     = "INITIAL_CLUSTER"
	initialClusterFmt     = "%s=http://localhost:%d"
	peerListenUrlsEnv     = "LISTEN_PEER_URLS"
	peerListenUrlsFmt     = "http://localhost:%d"
	peerAdvertiseUrlsEnv  = "INITIAL_ADVERTISE_PEER_URLS"
	peerAdvertiseUrlsFmt  = "http://localhost:%d"
	clientListenURLsEnv   = "LISTEN_CLIENT_URLS"
	clientListenURLFmt    = "http://127.0.0.1:%d"
	targetVersionEnv      = "TARGET_VERSION"
	targetStorageEnv      = "TARGET_STORAGE"
	etcdDataPrefixEnv     = "ETCD_DATA_PREFIX"
	etcdDataPrefixDefault = "/registry"
	ttlKeysDirectoryFmt   = "%s/events"
	etcdServerArgsEnv     = "ETCD_CREDS"
)

type migrateOpts struct {
	name              string
	port              uint64
	peerPort          uint64
	peerListenUrls    string
	peerAdvertiseUrls string
	binDir            string
	dataDir           string
	bundledVersions   []string
	supportedVersions SupportedVersions
	etcdDataPrefix    string
	ttlKeysDirectory  string
	initialCluster    string
	targetVersion     string
	targetStorage     string
	etcdServerArgs    string
	clientListenUrls  string
}

func registerFlags(flags *flag.FlagSet, opt *migrateOpts) {
	flags.StringVar(&opts.name, "name", "",
		"etcd cluster member name. If unset fallbacks to defaults to ETCD_NAME env, if unset defaults to etcd-<ETCD_HOSTNAME> env, if unset defaults to etcd-<HOSTNAME> env.")
	flags.Uint64Var(&opts.port, "port", 0,
		"etcd client port to use during migration operations. "+
			"This should be a different port than typically used by etcd to avoid clients accidentally connecting during upgrade/downgrade operations. "+
			"If unset default to 18629 or 18631 depending on <data-dir>.")
	flags.Uint64Var(&opts.peerPort, "peer-port", 0,
		"etcd peer port to use during migration operations. If unset defaults to 2380 or 2381 depending on <data-dir>.")
	flags.StringVar(&opts.peerListenUrls, "listen-peer-urls", "",
		"etcd --listen-peer-urls flag. If unset, fallbacks to LISTEN_PEER_URLS env and if unset defaults to http://localhost:<peer-port>.")
	flags.StringVar(&opts.peerAdvertiseUrls, "initial-advertise-peer-urls", "",
		"etcd --initial-advertise-peer-urls flag. If unset fallbacks to INITIAL_ADVERTISE_PEER_URLS env and if unset defaults to http://localhost:<peer-port>.")
	flags.StringVar(&opts.clientListenUrls, "listen-client-urls", "",
		"etcd --listen-client-urls flag. If unset, fallbacks to LISTEN_CLIENT_URLS env, and if unset defaults to http://127.0.0.1:<port>.")
	flags.StringVar(&opts.binDir, "bin-dir", "/usr/local/bin",
		"directory of etcd and etcdctl binaries, must contain etcd-<version> and etcdctl-<version> for each version listed in <bundled-versions>.")
	flags.StringVar(&opts.dataDir, "data-dir", "",
		"etcd data directory of etcd server to migrate. If unset fallbacks to DATA_DIRECTORY env.")
	flags.StringSliceVar(&opts.bundledVersions, "bundled-versions", supportedEtcdVersions,
		"comma separated list of etcd binary versions present under the bin-dir.")
	flags.StringVar(&opts.etcdDataPrefix, "etcd-data-prefix", "",
		"etcd key prefix under which all objects are kept. If unset fallbacks to ETCD_DATA_PREFIX env and if unset defaults to /registry.")
	flags.StringVar(&opts.ttlKeysDirectory, "ttl-keys-directory", "",
		"etcd key prefix under which all keys with TTLs are kept. Defaults to <etcd-data-prefix>/events")
	flags.StringVar(&opts.initialCluster, "initial-cluster", "",
		"comma separated list of name=endpoint pairs. If unset fallbacks to INITIAL_CLUSTER and if unset defaults to <etcd-name>=https://localhost:<peer-port>.")
	flags.StringVar(&opts.targetVersion, "target-version", "",
		"version of etcd to migrate to. Format must be <major>.<minor>.<patch>. If unset fallbacks to TARGET_VERSION env.")
	flags.StringVar(&opts.targetStorage, "target-storage", "",
		"storage version of etcd to migrate to, one of: etcd2, etcd3. If unset fallbacks to TARGET_STORAGE env.")
	flags.StringVar(&opts.etcdServerArgs, "etcd-server-extra-args", "",
		"additional etcd server args for starting etcd servers during migration steps, need to set TLS certs flags for multi-member clusters using mTLS for communication. "+
			"If unset fallbacks to ETCD_CREDS env.")
}

func lookupEnv(env string) (string, error) {
	result, ok := os.LookupEnv(env)
	if !ok || len(result) == 0 {
		return result, fmt.Errorf("%s variable unset - expected failure", env)
	}
	return result, nil
}

func fallbackToEnv(flag, env string) (string, error) {
	klog.Infof("--%s unset - falling back to %s variable", flag, env)
	return lookupEnv(env)
}

func fallbackToEnvWithDefault(flag, env, def string) string {
	if value, err := lookupEnv(env); err == nil {
		return value
	}
	klog.Warningf("%s variable for %s flag unset - defaulting to %s", env, flag, def)
	return def
}

func defaultName() (string, error) {
	if etcdName, err := lookupEnv(etcdNameEnv); err == nil {
		return etcdName, nil
	}
	klog.Warningf("%s variable unset - falling back to etcd-%s variable", etcdNameEnv, etcdHostnameEnv)
	if etcdHostname, err := lookupEnv(etcdHostnameEnv); err == nil {
		return fmt.Sprintf("etcd-%s", etcdHostname), nil
	}
	klog.Warningf("%s variable unset - falling back to etcd-%s variable", etcdHostnameEnv, hostnameEnv)
	if hostname, err := lookupEnv(hostnameEnv); err == nil {
		return fmt.Sprintf("etcd-%s", hostname), nil
	}
	return "", fmt.Errorf("defaulting --name failed due to all ETCD_NAME, ETCD_HOSTNAME and HOSTNAME unset")
}

func (opts *migrateOpts) validateAndDefault() error {
	var err error

	if opts.name == "" {
		klog.Infof("--name unset - falling back to %s variable", etcdNameEnv)
		if opts.name, err = defaultName(); err != nil {
			return err
		}
	}

	if opts.dataDir == "" {
		if opts.dataDir, err = fallbackToEnv("data-dir", dataDirEnv); err != nil {
			return err
		}
	}

	etcdEventsRE := regexp.MustCompile("event")
	if opts.port == 0 {
		if etcdEventsRE.MatchString(opts.dataDir) {
			opts.port = 18631
		} else {
			opts.port = 18629
		}
		klog.Infof("--port unset - defaulting to %d", opts.port)
	}
	if opts.peerPort == 0 {
		if etcdEventsRE.MatchString(opts.dataDir) {
			opts.peerPort = 2381
		} else {
			opts.peerPort = 2380
		}
		klog.Infof("--peer-port unset - defaulting to %d", opts.peerPort)
	}

	if opts.initialCluster == "" {
		def := fmt.Sprintf(initialClusterFmt, opts.name, opts.peerPort)
		opts.initialCluster = fallbackToEnvWithDefault("initial-cluster", initialClusterEnv, def)
	}

	if opts.peerListenUrls == "" {
		def := fmt.Sprintf(peerListenUrlsFmt, opts.peerPort)
		opts.peerListenUrls = fallbackToEnvWithDefault("listen-peer-urls", peerListenUrlsEnv, def)
	}

	if opts.peerAdvertiseUrls == "" {
		def := fmt.Sprintf(peerAdvertiseUrlsFmt, opts.peerPort)
		opts.peerAdvertiseUrls = fallbackToEnvWithDefault("initial-advertise-peer-urls", peerAdvertiseUrlsEnv, def)
	}

	if opts.clientListenUrls == "" {
		def := fmt.Sprintf(clientListenURLFmt, opts.port)
		opts.clientListenUrls = fallbackToEnvWithDefault("listen-client-urls", clientListenURLsEnv, def)
	}

	if opts.targetVersion == "" {
		if opts.targetVersion, err = fallbackToEnv("target-version", targetVersionEnv); err != nil {
			return err
		}
	}

	if opts.targetStorage == "" {
		if opts.targetStorage, err = fallbackToEnv("target-storage", targetStorageEnv); err != nil {
			return err
		}
	}

	if opts.etcdDataPrefix == "" {
		opts.etcdDataPrefix = fallbackToEnvWithDefault("etcd-data-prefix", etcdDataPrefixEnv, etcdDataPrefixDefault)
	}

	if opts.ttlKeysDirectory == "" {
		opts.ttlKeysDirectory = fmt.Sprintf(ttlKeysDirectoryFmt, opts.etcdDataPrefix)
		klog.Infof("--ttl-keys-directory unset - defaulting to %s", opts.ttlKeysDirectory)
	}

	if opts.etcdServerArgs == "" {
		opts.etcdServerArgs = fallbackToEnvWithDefault("etcd-server-extra-args", etcdServerArgsEnv, "")
	}

	if opts.supportedVersions, err = ParseSupportedVersions(opts.bundledVersions); err != nil {
		return fmt.Errorf("failed to parse --bundled-versions: %v", err)
	}

	if err := validateBundledVersions(opts.supportedVersions, opts.binDir); err != nil {
		return fmt.Errorf("failed to validate that 'etcd-<version>' and 'etcdctl-<version>' binaries exist in --bin-dir '%s' for all --bundled-versions '%s': %v",
			opts.binDir, strings.Join(opts.bundledVersions, ","), err)
	}
	return nil
}

// validateBundledVersions checks that 'etcd-<version>' and 'etcdctl-<version>' binaries exist in the binDir
// for each version in the bundledVersions list.
func validateBundledVersions(bundledVersions SupportedVersions, binDir string) error {
	for _, v := range bundledVersions {
		for _, binaryName := range []string{"etcd", "etcdctl"} {
			fn := filepath.Join(binDir, fmt.Sprintf("%s-%s", binaryName, v))
			if _, err := os.Stat(fn); err != nil {
				return fmt.Errorf("failed to validate '%s' binary exists for bundled-version '%s': %v", fn, v, err)
			}

		}
	}
	return nil
}
