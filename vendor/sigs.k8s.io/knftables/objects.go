/*
Copyright 2023 The Kubernetes Authors.

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

package knftables

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func parseInt(numbersOnly string) *int {
	i64, _ := strconv.ParseInt(numbersOnly, 10, 64)
	i := int(i64)
	return &i
}

func parseUint(numbersOnly string) *uint64 {
	ui64, _ := strconv.ParseUint(numbersOnly, 10, 64)
	return &ui64
}

// getComment parses a match for the commentGroup regexp (below). To distinguish between empty comment and no comment,
// we capture comment with double quotes.
func getComment(commentGroup string) *string {
	if commentGroup == "" {
		return nil
	}
	noQuotes := strings.Trim(commentGroup, "\"")
	return &noQuotes
}

var commentGroup = `(".*")`
var noSpaceGroup = `([^ ]*)`
var numberGroup = `([0-9]*)`

// Object implementation for Table
func (table *Table) validate(verb verb) error {
	switch verb {
	case addVerb, createVerb, flushVerb:
		if table.Handle != nil {
			return fmt.Errorf("cannot specify Handle in %s operation", verb)
		}
	case deleteVerb:
		// Handle can be nil or non-nil
	default:
		return fmt.Errorf("%s is not implemented for tables", verb)
	}

	return nil
}

func (table *Table) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	// Special case for delete-by-handle
	if verb == deleteVerb && table.Handle != nil {
		fmt.Fprintf(writer, "delete table %s handle %d", ctx.family, *table.Handle)
		return
	}

	// All other cases refer to the table by name
	fmt.Fprintf(writer, "%s table %s %s", verb, ctx.family, ctx.table)
	if verb == addVerb || verb == createVerb {
		if table.Comment != nil && !ctx.noObjectComments {
			fmt.Fprintf(writer, " { comment %q ; }", *table.Comment)
		}
	}
	fmt.Fprintf(writer, "\n")
}

var tableRegexp = regexp.MustCompile(fmt.Sprintf(
	`(?:{ comment %s ; })?`, commentGroup))

func (table *Table) parse(line string) error {
	match := tableRegexp.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("failed parsing table add command")
	}
	table.Comment = getComment(match[1])
	return nil
}

// Object implementation for Chain
func (chain *Chain) validate(verb verb) error {
	if chain.Hook == nil {
		if chain.Type != nil || chain.Priority != nil {
			return fmt.Errorf("regular chain %q must not specify Type or Priority", chain.Name)
		}
		if chain.Device != nil {
			return fmt.Errorf("regular chain %q must not specify Device", chain.Name)
		}
	} else {
		if chain.Type == nil || chain.Priority == nil {
			return fmt.Errorf("base chain %q must specify Type and Priority", chain.Name)
		}
	}

	switch verb {
	case addVerb, createVerb, flushVerb:
		if chain.Name == "" {
			return fmt.Errorf("no name specified for chain")
		}
		if chain.Handle != nil {
			return fmt.Errorf("cannot specify Handle in %s operation", verb)
		}
	case deleteVerb:
		if chain.Name == "" && chain.Handle == nil {
			return fmt.Errorf("must specify either name or handle")
		}
	default:
		return fmt.Errorf("%s is not implemented for chains", verb)
	}

	return nil
}

func (chain *Chain) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	// Special case for delete-by-handle
	if verb == deleteVerb && chain.Handle != nil {
		fmt.Fprintf(writer, "delete chain %s %s handle %d", ctx.family, ctx.table, *chain.Handle)
		return
	}

	fmt.Fprintf(writer, "%s chain %s %s %s", verb, ctx.family, ctx.table, chain.Name)
	if verb == addVerb || verb == createVerb {
		if chain.Type != nil || (chain.Comment != nil && !ctx.noObjectComments) {
			fmt.Fprintf(writer, " {")

			if chain.Type != nil {
				fmt.Fprintf(writer, " type %s hook %s", *chain.Type, *chain.Hook)
				if chain.Device != nil {
					fmt.Fprintf(writer, " device %q", *chain.Device)
				}

				// Parse the priority to a number if we can, because older
				// versions of nft don't accept certain named priorities
				// in all contexts (eg, "dstnat" priority in the "output"
				// hook).
				if priority, err := ParsePriority(ctx.family, string(*chain.Priority)); err == nil {
					fmt.Fprintf(writer, " priority %d ;", priority)
				} else {
					fmt.Fprintf(writer, " priority %s ;", *chain.Priority)
				}
			}
			if chain.Comment != nil && !ctx.noObjectComments {
				fmt.Fprintf(writer, " comment %q ;", *chain.Comment)
			}

			fmt.Fprintf(writer, " }")
		}
	}

	fmt.Fprintf(writer, "\n")
}

// groups in []: [1]%s(?: {(?: type [2]%s hook [3]%s(?: device "[4]%s")(?: priority [5]%s ;))(?: comment [6]%s ;) })
var chainRegexp = regexp.MustCompile(fmt.Sprintf(
	`%s(?: {(?: type %s hook %s(?: device "%s")?(?: priority %s ;))?(?: comment %s ;)? })?`,
	noSpaceGroup, noSpaceGroup, noSpaceGroup, noSpaceGroup, noSpaceGroup, commentGroup))

func (chain *Chain) parse(line string) error {
	match := chainRegexp.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("failed parsing chain add command")
	}
	chain.Name = match[1]
	chain.Comment = getComment(match[6])
	if match[2] != "" {
		chain.Type = (*BaseChainType)(&match[2])
	}
	if match[3] != "" {
		chain.Hook = (*BaseChainHook)(&match[3])
	}
	if match[4] != "" {
		chain.Device = &match[4]
	}
	if match[5] != "" {
		chain.Priority = (*BaseChainPriority)(&match[5])
	}
	return nil
}

// Object implementation for Rule
func (rule *Rule) validate(verb verb) error {
	if rule.Chain == "" {
		return fmt.Errorf("no chain name specified for rule")
	}

	if rule.Index != nil && rule.Handle != nil {
		return fmt.Errorf("cannot specify both Index and Handle")
	}

	switch verb {
	case addVerb, insertVerb:
		if rule.Rule == "" {
			return fmt.Errorf("no rule specified")
		}
	case replaceVerb:
		if rule.Rule == "" {
			return fmt.Errorf("no rule specified")
		}
		if rule.Handle == nil {
			return fmt.Errorf("must specify Handle with %s", verb)
		}
	case deleteVerb:
		if rule.Handle == nil {
			return fmt.Errorf("must specify Handle with %s", verb)
		}
	default:
		return fmt.Errorf("%s is not implemented for rules", verb)
	}

	return nil
}

func (rule *Rule) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	fmt.Fprintf(writer, "%s rule %s %s %s", verb, ctx.family, ctx.table, rule.Chain)
	if rule.Index != nil {
		fmt.Fprintf(writer, " index %d", *rule.Index)
	} else if rule.Handle != nil {
		fmt.Fprintf(writer, " handle %d", *rule.Handle)
	}

	switch verb {
	case addVerb, insertVerb, replaceVerb:
		fmt.Fprintf(writer, " %s", rule.Rule)

		if rule.Comment != nil {
			fmt.Fprintf(writer, " comment %q", *rule.Comment)
		}
	}

	fmt.Fprintf(writer, "\n")
}

// groups in []: [1]%s(?: index [2]%s)?(?: handle [3]%s)? [4]([^"]*)(?: comment [5]%s)?$
var ruleRegexp = regexp.MustCompile(fmt.Sprintf(
	`%s(?: index %s)?(?: handle %s)? ([^"]*)(?: comment %s)?$`,
	noSpaceGroup, numberGroup, numberGroup, commentGroup))

func (rule *Rule) parse(line string) error {
	match := ruleRegexp.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("failed parsing rule add command")
	}
	rule.Chain = match[1]
	rule.Rule = match[4]
	rule.Comment = getComment(match[5])
	if match[2] != "" {
		rule.Index = parseInt(match[2])
	}
	if match[3] != "" {
		rule.Handle = parseInt(match[3])
	}
	return nil
}

// Object implementation for Set
func (set *Set) validate(verb verb) error {
	switch verb {
	case addVerb, createVerb:
		if (set.Type == "" && set.TypeOf == "") || (set.Type != "" && set.TypeOf != "") {
			return fmt.Errorf("set must specify either Type or TypeOf")
		}
		if set.Handle != nil {
			return fmt.Errorf("cannot specify Handle in %s operation", verb)
		}
		fallthrough
	case flushVerb:
		if set.Name == "" {
			return fmt.Errorf("no name specified for set")
		}
	case deleteVerb:
		if set.Name == "" && set.Handle == nil {
			return fmt.Errorf("must specify either name or handle")
		}
	default:
		return fmt.Errorf("%s is not implemented for sets", verb)
	}

	return nil
}

func (set *Set) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	// Special case for delete-by-handle
	if verb == deleteVerb && set.Handle != nil {
		fmt.Fprintf(writer, "delete set %s %s handle %d", ctx.family, ctx.table, *set.Handle)
		return
	}

	fmt.Fprintf(writer, "%s set %s %s %s", verb, ctx.family, ctx.table, set.Name)
	if verb == addVerb || verb == createVerb {
		fmt.Fprintf(writer, " {")

		if set.Type != "" {
			fmt.Fprintf(writer, " type %s ;", set.Type)
		} else {
			fmt.Fprintf(writer, " typeof %s ;", set.TypeOf)
		}

		if len(set.Flags) != 0 {
			fmt.Fprintf(writer, " flags ")
			for i := range set.Flags {
				if i > 0 {
					fmt.Fprintf(writer, ",")
				}
				fmt.Fprintf(writer, "%s", set.Flags[i])
			}
			fmt.Fprintf(writer, " ;")
		}

		if set.Timeout != nil {
			fmt.Fprintf(writer, " timeout %ds ;", int64(set.Timeout.Seconds()))
		}
		if set.GCInterval != nil {
			fmt.Fprintf(writer, " gc-interval %ds ;", int64(set.GCInterval.Seconds()))
		}
		if set.Size != nil {
			fmt.Fprintf(writer, " size %d ;", *set.Size)
		}
		if set.Policy != nil {
			fmt.Fprintf(writer, " policy %s ;", *set.Policy)
		}
		if set.AutoMerge != nil && *set.AutoMerge {
			fmt.Fprintf(writer, " auto-merge ;")
		}

		if set.Comment != nil && !ctx.noObjectComments {
			fmt.Fprintf(writer, " comment %q ;", *set.Comment)
		}

		fmt.Fprintf(writer, " }")
	}

	fmt.Fprintf(writer, "\n")
}

func (set *Set) parse(line string) error {
	match := setRegexp.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("failed parsing set add command")
	}
	set.Name, set.Type, set.TypeOf, set.Flags, set.Timeout, set.GCInterval,
		set.Size, set.Policy, set.Comment, set.AutoMerge = parseMapAndSetProps(match)
	return nil
}

// Object implementation for Map
func (mapObj *Map) validate(verb verb) error {
	switch verb {
	case addVerb, createVerb:
		if (mapObj.Type == "" && mapObj.TypeOf == "") || (mapObj.Type != "" && mapObj.TypeOf != "") {
			return fmt.Errorf("map must specify either Type or TypeOf")
		}
		if mapObj.Handle != nil {
			return fmt.Errorf("cannot specify Handle in %s operation", verb)
		}
		fallthrough
	case flushVerb:
		if mapObj.Name == "" {
			return fmt.Errorf("no name specified for map")
		}
	case deleteVerb:
		if mapObj.Name == "" && mapObj.Handle == nil {
			return fmt.Errorf("must specify either name or handle")
		}
	default:
		return fmt.Errorf("%s is not implemented for maps", verb)
	}

	return nil
}

func (mapObj *Map) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	// Special case for delete-by-handle
	if verb == deleteVerb && mapObj.Handle != nil {
		fmt.Fprintf(writer, "delete map %s %s handle %d", ctx.family, ctx.table, *mapObj.Handle)
		return
	}

	fmt.Fprintf(writer, "%s map %s %s %s", verb, ctx.family, ctx.table, mapObj.Name)
	if verb == addVerb || verb == createVerb {
		fmt.Fprintf(writer, " {")

		if mapObj.Type != "" {
			fmt.Fprintf(writer, " type %s ;", mapObj.Type)
		} else {
			fmt.Fprintf(writer, " typeof %s ;", mapObj.TypeOf)
		}

		if len(mapObj.Flags) != 0 {
			fmt.Fprintf(writer, " flags ")
			for i := range mapObj.Flags {
				if i > 0 {
					fmt.Fprintf(writer, ",")
				}
				fmt.Fprintf(writer, "%s", mapObj.Flags[i])
			}
			fmt.Fprintf(writer, " ;")
		}

		if mapObj.Timeout != nil {
			fmt.Fprintf(writer, " timeout %ds ;", int64(mapObj.Timeout.Seconds()))
		}
		if mapObj.GCInterval != nil {
			fmt.Fprintf(writer, " gc-interval %ds ;", int64(mapObj.GCInterval.Seconds()))
		}
		if mapObj.Size != nil {
			fmt.Fprintf(writer, " size %d ;", *mapObj.Size)
		}
		if mapObj.Policy != nil {
			fmt.Fprintf(writer, " policy %s ;", *mapObj.Policy)
		}

		if mapObj.Comment != nil && !ctx.noObjectComments {
			fmt.Fprintf(writer, " comment %q ;", *mapObj.Comment)
		}

		fmt.Fprintf(writer, " }")
	}

	fmt.Fprintf(writer, "\n")
}

func (mapObj *Map) parse(line string) error {
	match := mapRegexp.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("failed parsing map add command")
	}
	mapObj.Name, mapObj.Type, mapObj.TypeOf, mapObj.Flags, mapObj.Timeout, mapObj.GCInterval,
		mapObj.Size, mapObj.Policy, mapObj.Comment, _ = parseMapAndSetProps(match)
	return nil
}

var autoMergeProp = `( auto-merge ;)?`

// groups in []:  [1]%s {(?: [2](type|typeof) [3]([^;]*)) ;(?: flags [4]([^;]*) ;)?(?: timeout [5]%ss ;)?(?: gc-interval [6]%ss ;)?(?: size [7]%s ;)?(?: policy [8]%s ;)?[9]%s(?: comment [10]%s ;)? }
var mapOrSet = `%s {(?: (type|typeof) ([^;]*)) ;(?: flags ([^;]*) ;)?(?: timeout %ss ;)?(?: gc-interval %ss ;)?(?: size %s ;)?(?: policy %s ;)?%s(?: comment %s ;)? }`
var mapRegexp = regexp.MustCompile(fmt.Sprintf(mapOrSet, noSpaceGroup, numberGroup, numberGroup, noSpaceGroup, noSpaceGroup, "", commentGroup))
var setRegexp = regexp.MustCompile(fmt.Sprintf(mapOrSet, noSpaceGroup, numberGroup, numberGroup, noSpaceGroup, noSpaceGroup, autoMergeProp, commentGroup))

func parseMapAndSetProps(match []string) (name string, typeProp string, typeOf string, flags []SetFlag,
	timeout *time.Duration, gcInterval *time.Duration, size *uint64, policy *SetPolicy, comment *string, autoMerge *bool) {
	name = match[1]
	// set and map have different number of match groups, but comment is always the last
	comment = getComment(match[len(match)-1])
	if match[2] == "type" {
		typeProp = match[3]
	} else {
		typeOf = match[3]
	}
	if match[4] != "" {
		flags = parseSetFlags(match[4])
	}
	if match[5] != "" {
		timeoutObj, _ := time.ParseDuration(match[5] + "s")
		timeout = &timeoutObj
	}
	if match[6] != "" {
		gcIntervalObj, _ := time.ParseDuration(match[6] + "s")
		gcInterval = &gcIntervalObj
	}
	if match[7] != "" {
		size = parseUint(match[7])
	}
	if match[8] != "" {
		policy = (*SetPolicy)(&match[8])
	}
	if len(match) > 10 {
		// set
		if match[9] != "" {
			autoMergeObj := true
			autoMerge = &autoMergeObj
		}
	}
	return
}

func parseSetFlags(s string) []SetFlag {
	var res []SetFlag
	for _, flag := range strings.Split(s, ",") {
		res = append(res, SetFlag(flag))
	}
	return res
}

// Object implementation for Element
func (element *Element) validate(verb verb) error {
	if element.Map == "" && element.Set == "" {
		return fmt.Errorf("no set/map name specified for element")
	} else if element.Set != "" && element.Map != "" {
		return fmt.Errorf("element specifies both a set name and a map name")
	}

	if len(element.Key) == 0 {
		return fmt.Errorf("no key specified for element")
	}
	if element.Set != "" && len(element.Value) != 0 {
		return fmt.Errorf("map value specified for set element")
	}

	switch verb {
	case addVerb, createVerb:
		if element.Map != "" && len(element.Value) == 0 {
			return fmt.Errorf("no map value specified for map element")
		}
	case deleteVerb:
	default:
		return fmt.Errorf("%s is not implemented for elements", verb)
	}

	return nil
}

func (element *Element) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	name := element.Set
	if name == "" {
		name = element.Map
	}

	fmt.Fprintf(writer, "%s element %s %s %s { %s", verb, ctx.family, ctx.table, name,
		strings.Join(element.Key, " . "))

	if verb == addVerb || verb == createVerb {
		if element.Comment != nil {
			fmt.Fprintf(writer, " comment %q", *element.Comment)
		}

		if len(element.Value) != 0 {
			fmt.Fprintf(writer, " : %s", strings.Join(element.Value, " . "))
		}
	}

	fmt.Fprintf(writer, " }\n")
}

// groups in []: [1]%s { [2]([^:"]*)(?: comment [3]%s)? : [4](.*) }
var mapElementRegexp = regexp.MustCompile(fmt.Sprintf(
	`%s { ([^"]*)(?: comment %s)? : (.*) }`, noSpaceGroup, commentGroup))

// groups in []: [1]%s { [2]([^:"]*)(?: comment [3]%s)? }
var setElementRegexp = regexp.MustCompile(fmt.Sprintf(
	`%s { ([^"]*)(?: comment %s)? }`, noSpaceGroup, commentGroup))

func (element *Element) parse(line string) error {
	// try to match map element first, since it has more groups, and if it matches, then we can be sure
	// this is map element.
	match := mapElementRegexp.FindStringSubmatch(line)
	if match == nil {
		match = setElementRegexp.FindStringSubmatch(line)
		if match == nil {
			return fmt.Errorf("failed parsing element add command")
		}
	}
	element.Comment = getComment(match[3])
	mapOrSetName := match[1]
	element.Key = append(element.Key, strings.Split(match[2], " . ")...)
	if len(match) == 5 {
		// map regex matched
		element.Map = mapOrSetName
		element.Value = append(element.Value, strings.Split(match[4], " . ")...)
	} else {
		element.Set = mapOrSetName
	}
	return nil
}

// Object implementation for Flowtable
func (flowtable *Flowtable) validate(verb verb) error {
	switch verb {
	case addVerb, createVerb:
		if flowtable.Name == "" {
			return fmt.Errorf("no name specified for flowtable")
		}
		if flowtable.Handle != nil {
			return fmt.Errorf("cannot specify Handle in %s operation", verb)
		}
	case deleteVerb:
		if flowtable.Name == "" && flowtable.Handle == nil {
			return fmt.Errorf("must specify either name or handle")
		}
	default:
		return fmt.Errorf("%s is not implemented for flowtables", verb)
	}

	return nil
}

func (flowtable *Flowtable) writeOperation(verb verb, ctx *nftContext, writer io.Writer) {
	// Special case for delete-by-handle
	if verb == deleteVerb && flowtable.Handle != nil {
		fmt.Fprintf(writer, "delete flowtable %s %s handle %d", ctx.family, ctx.table, *flowtable.Handle)
		return
	}

	fmt.Fprintf(writer, "%s flowtable %s %s %s", verb, ctx.family, ctx.table, flowtable.Name)
	if verb == addVerb || verb == createVerb {
		fmt.Fprintf(writer, " {")

		if flowtable.Priority != nil {
			// since there is only one priority value allowed "filter" just use the value
			// provided and not try to parse it.
			fmt.Fprintf(writer, " hook ingress priority %s ;", *flowtable.Priority)
		}

		if len(flowtable.Devices) > 0 {
			fmt.Fprintf(writer, " devices = { %s } ;", strings.Join(flowtable.Devices, ", "))
		}

		fmt.Fprintf(writer, " }")
	}

	fmt.Fprintf(writer, "\n")
}

// nft add flowtable inet example_table example_flowtable { hook ingress priority filter ; devices = { eth0 };  }
var flowtableRegexp = regexp.MustCompile(fmt.Sprintf(
	`%s(?: {(?: hook ingress priority %s ;)(?: devices = {(.*)} ;) })?`,
	noSpaceGroup, noSpaceGroup))

func (flowtable *Flowtable) parse(line string) error {
	match := flowtableRegexp.FindStringSubmatch(line)
	if match == nil {
		return fmt.Errorf("failed parsing flowtableRegexp add command")
	}
	flowtable.Name = match[1]
	if match[2] != "" {
		flowtable.Priority = (*FlowtableIngressPriority)(&match[2])
	}
	// to avoid complex regular expressions the regex match everything between the brackets
	// to match a single interface or a comma separated list of interfaces, and it is postprocessed
	// here to remove the whitespaces.
	if match[3] != "" {
		devices := strings.Split(strings.TrimSpace(match[3]), ",")
		for i := range devices {
			devices[i] = strings.TrimSpace(devices[i])
		}
		if len(devices) > 0 {
			flowtable.Devices = devices
		}
	}
	return nil
}
