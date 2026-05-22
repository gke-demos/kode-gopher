/*
Copyright 2026 Google LLC

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

import "github.com/traefik/yaegi/interp"

// Symbols is the package-global the generated `yaegi extract` files
// populate via init(). With no build tags, it stays empty (stdlib-only
// baseline). Building with -tags=rest pulls in the storage/v1 + option
// extracts; -tags=grpc pulls in cloud.google.com/go/storage + iterator.
var Symbols = interp.Exports{}

func extraSymbols() interp.Exports { return Symbols }
