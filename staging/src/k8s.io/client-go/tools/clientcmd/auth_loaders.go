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
	"encoding/json"
	"io"
	"io/ioutil"
	"os"

	clientauth "k8s.io/client-go/tools/auth"
	"k8s.io/kubernetes/pkg/util/prompt"
)

// AuthLoaders are used to build clientauth.Info objects.
type AuthLoader interface {
	// LoadAuth takes a path to a config file and can then do anything it needs in order to return a valid clientauth.Info
	LoadAuth(path string) (*clientauth.Info, error)
}

// default implementation of an AuthLoader
type defaultAuthLoader struct{}

// LoadAuth for defaultAuthLoader simply delegates to clientauth.LoadFromFile
func (*defaultAuthLoader) LoadAuth(path string) (*clientauth.Info, error) {
	return clientauth.LoadFromFile(path)
}

type PromptingAuthLoader struct {
	reader io.Reader
}

// LoadAuth parses an AuthInfo object from a file path. It prompts user and creates file if it doesn't exist.
func (a *PromptingAuthLoader) LoadAuth(path string) (*clientauth.Info, error) {
	// Prompt for user/pass and write a file if none exists.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		authPtr, err := a.Prompt()
		auth := *authPtr
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(auth)
		if err != nil {
			return &auth, err
		}
		err = ioutil.WriteFile(path, data, 0600)
		return &auth, err
	}
	authPtr, err := clientauth.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	return authPtr, nil
}

// Prompt pulls the user and password from a reader
func (a *PromptingAuthLoader) Prompt() (auth *clientauth.Info, err error) {
	prompter := prompt.NewPrompter(a.reader)
	auth = &clientauth.Info{}

	auth.User, err = prompter.Prompt("Username", prompt.ShowEcho, prompt.DontMask)
	if err != nil {
		return nil, err
	}

	auth.Password, err = prompter.Prompt("Password", prompt.DontShowEcho, prompt.Mask)
	if err != nil {
		return nil, err
	}
	return auth, nil
}

// NewPromptingAuthLoader is an AuthLoader that parses an AuthInfo object from a file path. It prompts user and creates file if it doesn't exist.
func NewPromptingAuthLoader(reader io.Reader) *PromptingAuthLoader {
	return &PromptingAuthLoader{reader}
}

// NewDefaultAuthLoader returns a default implementation of an AuthLoader that only reads from a config file
func NewDefaultAuthLoader() AuthLoader {
	return &defaultAuthLoader{}
}
