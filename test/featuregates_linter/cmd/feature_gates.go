/*
Copyright 2024 The Kubernetes Authors.

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

package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/google/go-cmp/cmp"
	"github.com/spf13/cobra"

	"k8s.io/apimachinery/pkg/util/version"
	yaml "sigs.k8s.io/yaml/goyaml.v2"
)

var (
	alphabeticalOrder          bool
	k8RootPath                 string
	unversionedFeatureListFile = "test/featuregates_linter/test_data/unversioned_feature_list.yaml"
	versionedFeatureListFile   = "test/featuregates_linter/test_data/versioned_feature_list.yaml"
)

const (
	featureGatePkg = "\"k8s.io/component-base/featuregate\""
)

type featureSpec struct {
	Default       bool   `yaml:"default" json:"default"`
	LockToDefault bool   `yaml:"lockToDefault" json:"lockToDefault"`
	PreRelease    string `yaml:"preRelease" json:"preRelease"`
	Version       string `yaml:"version" json:"version"`
}

type featureInfo struct {
	Name           string        `yaml:"name" json:"name"`
	FullName       string        `yaml:"-" json:"-"`
	VersionedSpecs []featureSpec `yaml:"versionedSpecs" json:"versionedSpecs"`
}

// NewFeatureGatesCommand returns the cobra command for "feature-gates".
func NewFeatureGatesCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "feature-gates <subcommand>",
		Short: "Commands related to feature gate verifications and updates",
	}
	defaultRootPath, err := filepath.Abs(".")
	if err != nil {
		panic(err)
	}
	cmd.Flags().StringVar(&k8RootPath, "root-path", defaultRootPath, "absolute path of the k8s repository")

	cmd.AddCommand(NewVerifyFeatureListCommand())
	cmd.AddCommand(NewUpdateFeatureListCommand())
	return cmd
}

func NewVerifyFeatureListCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:   "verify",
		Short: "Verifies feature list files are up to date.",
		Run:   verifyFeatureListFunc,
	}
	cmd.Flags().BoolVar(&alphabeticalOrder, "alphabetical-order", false, "if true, verify all features in any FeatureSpec map are ordered aphabetically")
	return &cmd
}

func NewUpdateFeatureListCommand() *cobra.Command {
	cmd := cobra.Command{
		Use:   "update",
		Short: "updates feature list files.",
		Run:   updateFeatureListFunc,
	}
	return &cmd
}

func verifyFeatureListFunc(cmd *cobra.Command, args []string) {
	if err := verifyOrUpdateFeatureList(k8RootPath, unversionedFeatureListFile, false, false); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to verify feature list: \n%s", err)
		os.Exit(1)
	}
	if err := verifyOrUpdateFeatureList(k8RootPath, versionedFeatureListFile, false, true); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to verify versioned feature list: \n%s", err)
		os.Exit(1)
	}
}

func updateFeatureListFunc(cmd *cobra.Command, args []string) {
	if err := verifyOrUpdateFeatureList(k8RootPath, unversionedFeatureListFile, true, false); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update feature list: \n%s", err)
		os.Exit(1)
	}
	if err := verifyOrUpdateFeatureList(k8RootPath, versionedFeatureListFile, true, true); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to update versioned feature list: \n%s", err)
		os.Exit(1)
	}
}

// verifyOrUpdateFeatureList walks all the files under pkg/ and staging/ to find the list of all the features in
// map[featuregate.Feature]featuregate.FeatureSpec or map[featuregate.Feature]featuregate.VersionedSpecs.
// It will then update the feature list in featureListFile, or verifies there is no change from the existing list.
func verifyOrUpdateFeatureList(rootPath, featureListFile string, update, versioned bool) error {
	featureList := []featureInfo{}
	features, err := searchPathForFeatures(filepath.Join(rootPath, "pkg"), versioned)
	if err != nil {
		return err
	}
	featureList = append(featureList, features...)

	features, err = searchPathForFeatures(filepath.Join(rootPath, "staging"), versioned)
	if err != nil {
		return err
	}
	featureList = append(featureList, features...)

	sort.Slice(featureList, func(i, j int) bool {
		return strings.ToLower(featureList[i].Name) < strings.ToLower(featureList[j].Name)
	})
	featureList, err = dedupeFeatureList(featureList)
	if err != nil {
		return err
	}

	filePath := filepath.Join(rootPath, featureListFile)
	baseFeatureListBytes, err := os.ReadFile(filePath)
	if err != nil {
		return err
	}

	baseFeatureList := []featureInfo{}
	err = yaml.Unmarshal(baseFeatureListBytes, &baseFeatureList)
	if err != nil {
		return err
	}

	// only feature deletion is allowed for unversioned features.
	// all new features or feature updates should be migrated to versioned feature gate.
	// https://github.com/kubernetes/kubernetes/issues/125031
	if !versioned {
		if err := verifyFeatureDeletionOnly(featureList, baseFeatureList); err != nil {
			return err
		}
	}

	featureListBytes, err := yaml.Marshal(featureList)
	if err != nil {
		return err
	}
	if update {
		return os.WriteFile(filePath, featureListBytes, 0644)
	}

	if diff := cmp.Diff(featureListBytes, baseFeatureListBytes); diff != "" {
		if versioned {
			return fmt.Errorf("detected diff in versioned feature list (%s), diff: \n%s", versionedFeatureListFile, diff)
		} else {
			return fmt.Errorf("detected diff in unversioned feature list (%s), diff: \n%s", unversionedFeatureListFile, diff)
		}
	}
	return nil
}

func dedupeFeatureList(featureList []featureInfo) ([]featureInfo, error) {
	if featureList == nil || len(featureList) < 1 {
		return featureList, nil
	}
	last := featureList[0]
	// clean up FullName field for the final output
	last.FullName = ""
	deduped := []featureInfo{last}
	for i := 1; i < len(featureList); i++ {
		f := featureList[i]
		if f.Name == last.Name {
			// if it is a duplicate feature, verify the lifecycles are the same
			if !reflect.DeepEqual(last.VersionedSpecs, f.VersionedSpecs) {
				return deduped, fmt.Errorf("multiple conflicting specs found for feature:%s, [\n%v, \n%v]", last.Name, last.VersionedSpecs, f.VersionedSpecs)
			}
			continue
		}
		last = f
		last.FullName = ""
		deduped = append(deduped, last)

	}
	return deduped, nil
}

func verifyFeatureDeletionOnly(newFeatureList []featureInfo, oldFeatureList []featureInfo) error {
	oldFeatureSet := make(map[string]*featureInfo)
	for _, f := range oldFeatureList {
		oldFeatureSet[f.Name] = &f
	}
	newFeatures := []string{}
	for _, f := range newFeatureList {
		oldSpecs, found := oldFeatureSet[f.Name]
		if !found {
			newFeatures = append(newFeatures, f.Name)
		} else if !reflect.DeepEqual(*oldSpecs, f) {
			return fmt.Errorf("feature %s changed with diff: %s", f.Name, cmp.Diff(*oldSpecs, f))
		}
	}
	if len(newFeatures) > 0 {
		return fmt.Errorf("new features added to FeatureSpec map! %v\nPlease add new features through VersionedSpecs map ONLY! ", newFeatures)
	}
	return nil
}

func searchPathForFeatures(path string, versioned bool) ([]featureInfo, error) {
	allFeatures := []featureInfo{}
	// Create a FileSet to work with
	fset := token.NewFileSet()
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if strings.HasPrefix(path, "vendor") || strings.HasPrefix(path, "_") {
			return filepath.SkipDir
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		features, parseErr := extractFeatureInfoListFromFile(fset, path, versioned)
		if parseErr != nil {
			return parseErr
		}
		allFeatures = append(allFeatures, features...)
		return nil
	})
	return allFeatures, err
}

// extractFeatureInfoListFromFile extracts info all the the features from
// map[featuregate.Feature]featuregate.FeatureSpec or map[featuregate.Feature]featuregate.VersionedSpecs from the given file.
func extractFeatureInfoListFromFile(fset *token.FileSet, filePath string, versioned bool) (allFeatures []featureInfo, err error) {
	// Parse the file and create an AST
	absFilePath, err := filepath.Abs(filePath)
	if err != nil {
		return allFeatures, err
	}
	file, err := parser.ParseFile(fset, absFilePath, nil, parser.AllErrors)
	if err != nil {
		return allFeatures, err
	}
	aliasMap := importAliasMap(file.Imports)
	// any file containing features should have imported the featuregate pkg.
	if _, ok := aliasMap[featureGatePkg]; !ok {
		return allFeatures, err
	}
	variables := globalVariableDeclarations(file)

	for _, d := range file.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.CONST || gd.Tok == token.VAR) {
			for _, spec := range gd.Specs {
				if vspec, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range vspec.Names {
						for _, value := range vspec.Values {
							features, err := extractFeatureInfoList(filePath, value, aliasMap, variables, versioned)
							if err != nil {
								return allFeatures, err
							}
							if len(features) > 0 {
								fmt.Printf("found %d features in FeatureSpecMap var %s in file: %s\n", len(features), name, filePath)
								allFeatures = append(allFeatures, features...)
							}
						}
					}
				}
			}
		}
		if fd, ok := d.(*ast.FuncDecl); ok {
			for _, stmt := range fd.Body.List {
				if st, ok := stmt.(*ast.ReturnStmt); ok {
					for _, value := range st.Results {
						features, err := extractFeatureInfoList(filePath, value, aliasMap, variables, versioned)
						if err != nil {
							return allFeatures, err
						}
						if len(features) > 0 {
							fmt.Printf("found %d features in FeatureSpecMap of func %s in file: %s\n", len(features), fd.Name, filePath)
							allFeatures = append(allFeatures, features...)
						}
					}
				}
			}
		}
	}
	return
}

func getPkgPrefix(s string) string {
	if strings.Contains(s, ".") {
		return strings.Split(s, ".")[0]
	}
	return ""
}

func verifyAlphabeticOrder(keys []string, path string) error {
	keysSorted := make([]string, len(keys))
	copy(keysSorted, keys)
	sort.Slice(keysSorted, func(i, j int) bool {
		keyI := strings.ToLower(keysSorted[i])
		keyJ := strings.ToLower(keysSorted[j])
		if getPkgPrefix(keyI) == getPkgPrefix(keyJ) {
			return keyI < keyJ
		}
		return getPkgPrefix(keyI) < getPkgPrefix(keyJ)
	})
	if diff := cmp.Diff(keys, keysSorted); diff != "" {
		return fmt.Errorf("features in %s are not in alphabetic order, diff: %s", path, diff)
	}
	return nil
}

// extractFeatureInfoList extracts the info all the the features from
// map[featuregate.Feature]featuregate.FeatureSpec or map[featuregate.Feature]featuregate.VersionedSpecs.
func extractFeatureInfoList(filePath string, v ast.Expr, aliasMap map[string]string, variables map[string]ast.Expr, versioned bool) ([]featureInfo, error) {
	keys := []string{}
	features := []featureInfo{}
	cl, ok := v.(*ast.CompositeLit)
	if !ok {
		return features, nil
	}
	mt, ok := cl.Type.(*ast.MapType)
	if !ok {
		return features, nil
	}
	if !isFeatureSpecType(mt.Value, aliasMap, versioned) {
		return features, nil
	}
	for _, elt := range cl.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		info, err := parseFeatureInfo(variables, kv, versioned)
		if err != nil {
			return features, err
		}
		features = append(features, info)
		keys = append(keys, info.FullName)
	}
	if alphabeticalOrder {
		// verifies the features are sorted in the map
		if err := verifyAlphabeticOrder(keys, filePath); err != nil {
			return features, err
		}
	}
	return features, nil
}

func isFeatureSpecType(v ast.Expr, aliasMap map[string]string, versioned bool) bool {
	typeName := "FeatureSpec"
	if versioned {
		typeName = "VersionedSpecs"
	}
	alias, ok := aliasMap[featureGatePkg]
	if ok {
		typeName = alias + "." + typeName
	}
	return identifierName(v, false) == typeName
}

func parseFeatureInfo(variables map[string]ast.Expr, kv *ast.KeyValueExpr, versioned bool) (featureInfo, error) {
	info := featureInfo{
		Name:           identifierName(kv.Key, true),
		FullName:       identifierName(kv.Key, false),
		VersionedSpecs: []featureSpec{},
	}
	specExps := []ast.Expr{}
	if versioned {
		if cl, ok := kv.Value.(*ast.CompositeLit); ok {
			specExps = append(specExps, cl.Elts...)
		}
	} else {
		specExps = append(specExps, kv.Value)
	}
	for _, specExp := range specExps {
		spec, err := parseFeatureSpec(variables, specExp)
		if err != nil {
			return info, err
		}
		info.VersionedSpecs = append(info.VersionedSpecs, spec)
	}
	// verify FeatureSpec in VersionedSpecs are ordered by version.
	if len(info.VersionedSpecs) > 1 {
		specsSorted := make([]featureSpec, len(info.VersionedSpecs))
		copy(specsSorted, info.VersionedSpecs)
		sort.Slice(specsSorted, func(i, j int) bool {
			verI := version.MustParse(specsSorted[i].Version)
			verJ := version.MustParse(specsSorted[j].Version)
			return verI.LessThan(verJ)
		})
		if diff := cmp.Diff(info.VersionedSpecs, specsSorted); diff != "" {
			return info, fmt.Errorf("VersionedSpecs in feature %s are not ordered by version, diff: %s", info.Name, diff)
		}
	}
	return info, nil
}

func parseFeatureSpec(variables map[string]ast.Expr, v ast.Expr) (featureSpec, error) {
	spec := featureSpec{}
	cl, ok := v.(*ast.CompositeLit)
	if !ok {
		return spec, fmt.Errorf("expect FeatureSpec to be a CompositeLit")
	}
	for _, elt := range cl.Elts {
		switch eltType := elt.(type) {
		case *ast.KeyValueExpr:
			key := identifierName(eltType.Key, true)
			switch key {
			case "Default":
				boolValue, err := parseBool(variables, eltType.Value)
				if err != nil {
					return spec, err
				}
				spec.Default = boolValue

			case "LockToDefault":
				boolValue, err := parseBool(variables, eltType.Value)
				if err != nil {
					return spec, err
				}
				spec.LockToDefault = boolValue

			case "PreRelease":
				spec.PreRelease = identifierName(eltType.Value, true)

			case "Version":
				ver, err := parseVersion(eltType.Value)
				if err != nil {
					return spec, err
				}
				spec.Version = ver
			}

		default:
			return spec, fmt.Errorf("cannot parse FeatureSpec")

		}
	}
	return spec, nil
}

func parseVersion(v ast.Expr) (string, error) {
	fc, ok := v.(*ast.CallExpr)
	if !ok {
		return "", fmt.Errorf("expect FeatureSpec Version to be a function call")
	}
	funcName := identifierName(fc.Fun, true)
	switch funcName {
	case "MustParse":
		return basicStringLiteral(fc.Args[0])

	case "MajorMinor":
		major, err := basicIntLiteral(fc.Args[0])
		if err != nil {
			return "", err
		}
		minor, err := basicIntLiteral(fc.Args[1])
		return fmt.Sprintf("%d.%d", major, minor), err

	default:
		return "", fmt.Errorf("unrecognized function call in FeatureSpec Version")
	}
}
