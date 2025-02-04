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
	"go/token"
	"os/exec"
	"strconv"
	"strings"
)

var (
	// env configs
	GOOS string = findGOOS()
)

func findGOOS() string {
	goCmd := exec.Command("go", "env", "GOOS")
	out, err := goCmd.CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("running `go env` failed: %v\n\n%s", err, string(out)))
	}
	if len(out) == 0 {
		panic("empty result from `go env GOOS`")
	}
	return string(out)
}

// identifierName returns the name of an identifier.
// if ignorePkg, only return the last part of the identifierName.
func identifierName(v ast.Expr, ignorePkg bool) string {
	if id, ok := v.(*ast.Ident); ok {
		return id.Name
	}
	if se, ok := v.(*ast.SelectorExpr); ok {
		if ignorePkg {
			return identifierName(se.Sel, ignorePkg)
		}
		return identifierName(se.X, ignorePkg) + "." + identifierName(se.Sel, ignorePkg)
	}
	return ""
}

// importAliasMap returns the mapping from pkg path to import alias.
func importAliasMap(imports []*ast.ImportSpec) map[string]string {
	m := map[string]string{}
	for _, im := range imports {
		var importAlias string
		if im.Name == nil {
			pathSegments := strings.Split(im.Path.Value, "/")
			importAlias = strings.Trim(pathSegments[len(pathSegments)-1], "\"")
		} else {
			importAlias = im.Name.String()
		}
		m[im.Path.Value] = importAlias
	}
	return m
}

func basicStringLiteral(v ast.Expr) (string, error) {
	bl, ok := v.(*ast.BasicLit)
	if !ok {
		return "", fmt.Errorf("cannot parse a non BasicLit to basicStringLiteral")
	}

	if bl.Kind != token.STRING {
		return "", fmt.Errorf("cannot parse a non STRING token to basicStringLiteral")
	}
	return strings.Trim(bl.Value, `"`), nil
}

func basicIntLiteral(v ast.Expr) (int64, error) {
	bl, ok := v.(*ast.BasicLit)
	if !ok {
		return 0, fmt.Errorf("cannot parse a non BasicLit to basicIntLiteral")
	}

	if bl.Kind != token.INT {
		return 0, fmt.Errorf("cannot parse a non INT token to basicIntLiteral")
	}
	value, err := strconv.ParseInt(bl.Value, 10, 64)
	if err != nil {
		return 0, err
	}
	return value, nil
}

func parseBool(variables map[string]ast.Expr, v ast.Expr) (bool, error) {
	ident := identifierName(v, false)
	switch ident {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		if varVal, ok := variables[ident]; ok {
			return parseBool(variables, varVal)
		}
		return false, fmt.Errorf("cannot parse %s into bool", ident)
	}
}

func globalVariableDeclarations(tree *ast.File) map[string]ast.Expr {
	consts := make(map[string]ast.Expr)
	for _, d := range tree.Decls {
		if gd, ok := d.(*ast.GenDecl); ok && (gd.Tok == token.CONST || gd.Tok == token.VAR) {
			for _, spec := range gd.Specs {
				if vspec, ok := spec.(*ast.ValueSpec); ok {
					for _, name := range vspec.Names {
						for _, value := range vspec.Values {
							consts[name.Name] = value
						}
					}
				}
			}
		}
	}
	return consts
}
