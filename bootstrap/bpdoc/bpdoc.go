package bpdoc

import (
	"fmt"
	"html/template"
	"reflect"
	"sort"
	"strings"

	"github.com/google/blueprint/proptools"
)

// Package contains the information about a package relevant to generating documentation.
type Package struct {
	// Name is the name of the package.
	Name string

	// Path is the full package path of the package as used in the primary builder.
	Path string

	// Text is the contents of the package comment documenting the module types in the package.
	Text string

	// ModuleTypes is a list of ModuleType objects that contain information about each module type that is
	// defined by the package.
	ModuleTypes []*ModuleType
}

// ModuleType contains the information about a module type that is relevant to generating documentation.
type ModuleType struct {
	// Name is the string that will appear in Blueprints files when defining a new module of
	// this type.
	Name string

	// PkgPath is the full package path of the package that contains the module type factory.
	PkgPath string

	// Text is the contents of the comment documenting the module type.
	Text template.HTML

	// PropertyStructs is a list of PropertyStruct objects that contain information about each
	// property struct that is used by the module type, containing all properties that are valid
	// for the module type.
	PropertyStructs []*PropertyStruct
}

type PropertyStruct struct {
	Name       string
	Text       string
	Properties []Property
}

type Property struct {
	Name       string
	OtherNames []string
	Type       string
	Tag        reflect.StructTag
	Text       template.HTML
	OtherTexts []template.HTML
	Properties []Property
	Default    string
	Anonymous  bool
}

func AllPackages(pkgFiles map[string][]string, moduleTypeNameFactories map[string]reflect.Value,
	moduleTypeNamePropertyStructs map[string][]interface{}) ([]*Package, error) {
	// Read basic info from the files to construct a Reader instance.
	r := NewReader(pkgFiles)

	pkgMap := map[string]*Package{}
	var pkgs []*Package
	// Scan through per-module-type property structs map.
	for mtName, propertyStructs := range moduleTypeNamePropertyStructs {
		// Construct ModuleType with the given info.
		mtInfo, err := assembleModuleTypeInfo(r, mtName, moduleTypeNameFactories[mtName], propertyStructs)
		if err != nil {
			return nil, err
		}
		// Some pruning work
		removeAnonymousProperties(mtInfo)
		removeEmptyPropertyStructs(mtInfo)
		collapseDuplicatePropertyStructs(mtInfo)
		collapseNestedPropertyStructs(mtInfo)

		// Add the ModuleInfo to the corresponding Package map/slice entries.
		pkg := pkgMap[mtInfo.PkgPath]
		if pkg == nil {
			var err error
			pkg, err = r.Package(mtInfo.PkgPath)
			if err != nil {
				return nil, err
			}
			pkgMap[mtInfo.PkgPath] = pkg
			pkgs = append(pkgs, pkg)
		}
		pkg.ModuleTypes = append(pkg.ModuleTypes, mtInfo)
	}

	// Sort ModuleTypes within each package.
	for _, pkg := range pkgs {
		sort.Slice(pkg.ModuleTypes, func(i, j int) bool { return pkg.ModuleTypes[i].Name < pkg.ModuleTypes[j].Name })
	}
	// Sort packages.
	sort.Slice(pkgs, func(i, j int) bool { return pkgs[i].Path < pkgs[j].Path })

	return pkgs, nil
}

func assembleModuleTypeInfo(r *Reader, name string, factory reflect.Value,
	propertyStructs []interface{}) (*ModuleType, error) {

	mt, err := r.ModuleType(name, factory)
	if err != nil {
		return nil, err
	}

	// Reader.ModuleType only fills basic information such as name and package path. Collect more info
	// from property struct data.
	for _, s := range propertyStructs {
		v := reflect.ValueOf(s).Elem()
		t := v.Type()

		ps, err := r.PropertyStruct(t.PkgPath(), t.Name(), v)

		if err != nil {
			return nil, err
		}
		ps.ExcludeByTag("blueprint", "mutated")
		for _, nestedProperty := range nestedPropertyStructs(v) {
			nestedName := nestedProperty.nestPoint
			nestedValue := nestedProperty.value
			nestedType := nestedValue.Type()

			// Ignore property structs with unexported or unnamed types
			if nestedType.PkgPath() == "" {
				continue
			}
			nested, err := r.PropertyStruct(nestedType.PkgPath(), nestedType.Name(), nestedValue)
			if err != nil {
				return nil, err
			}
			nested.ExcludeByTag("blueprint", "mutated")
			if nestedName == "" {
				ps.Nest(nested)
			} else {
				nestPoint := ps.GetByName(nestedName)
				if nestPoint == nil {
					return nil, fmt.Errorf("nesting point %q not found", nestedName)
				}
				nestPoint.Nest(nested)
			}

			if nestedProperty.anonymous {
				if nestedName != "" {
					nestedName += "."
				}
				nestedName += proptools.PropertyNameForField(nested.Name)
				nestedProp := ps.GetByName(nestedName)
				// Anonymous properties may have already been omitted, no need to ensure they are filtered later
				if nestedProp != nil {
					// Set property to anonymous to allow future filtering
					nestedProp.SetAnonymous()
				}
			}
		}
		mt.PropertyStructs = append(mt.PropertyStructs, ps)
	}

	return mt, nil
}

type nestedProperty struct {
	nestPoint string
	value     reflect.Value
	anonymous bool
}

func nestedPropertyStructs(s reflect.Value) []nestedProperty {
	ret := make([]nestedProperty, 0)
	var walk func(structValue reflect.Value, prefix string)
	walk = func(structValue reflect.Value, prefix string) {
		var nestStruct func(field reflect.StructField, value reflect.Value, fieldName string)
		nestStruct = func(field reflect.StructField, value reflect.Value, fieldName string) {
			nestPoint := prefix
			if field.Anonymous {
				nestPoint = strings.TrimSuffix(nestPoint, ".")
			} else {
				nestPoint = nestPoint + proptools.PropertyNameForField(fieldName)
			}
			ret = append(ret, nestedProperty{nestPoint: nestPoint, value: value, anonymous: field.Anonymous})
			if nestPoint != "" {
				nestPoint += "."
			}
			walk(value, nestPoint)
		}

		typ := structValue.Type()
		for i := 0; i < structValue.NumField(); i++ {
			field := typ.Field(i)
			if field.PkgPath != "" {
				// The field is not exported so just skip it.
				continue
			}
			if proptools.HasTag(field, "blueprint", "mutated") {
				continue
			}
			if proptools.IsConfigurable(field.Type) {
				// Don't recurse into configurable properties, they're structs but not property structs
				continue
			}

			fieldValue := structValue.Field(i)

			switch fieldValue.Kind() {
			case reflect.Bool, reflect.String, reflect.Slice, reflect.Int, reflect.Uint:
				// Nothing
			case reflect.Struct:
				nestStruct(field, fieldValue, field.Name)
			case reflect.Ptr, reflect.Interface:

				if !fieldValue.IsNil() {
					// We leave the pointer intact and zero out the struct that's
					// pointed to.
					elem := fieldValue.Elem()
					if fieldValue.Kind() == reflect.Interface {
						if elem.Kind() != reflect.Ptr {
							panic(fmt.Errorf("can't get type of field %q: interface "+
								"refers to a non-pointer", field.Name))
						}
						elem = elem.Elem()
					}
					if elem.Kind() == reflect.Struct {
						nestStruct(field, elem, field.Name)
					}
				}
			default:
				panic(fmt.Errorf("unexpected kind for property struct field %q: %s",
					field.Name, fieldValue.Kind()))
			}
		}
	}

	walk(s, "")
	return ret
}

// Remove any property structs that have no exported fields
func removeEmptyPropertyStructs(mt *ModuleType) {
	for i := 0; i < len(mt.PropertyStructs); i++ {
		if len(mt.PropertyStructs[i].Properties) == 0 {
			mt.PropertyStructs = append(mt.PropertyStructs[:i], mt.PropertyStructs[i+1:]...)
			i--
		}
	}
}

// Remove any property structs that are anonymous
func removeAnonymousProperties(mt *ModuleType) {
	var removeAnonymousProps func(props []Property) []Property
	removeAnonymousProps = func(props []Property) []Property {
		newProps := make([]Property, 0, len(props))
		for _, p := range props {
			if p.Anonymous {
				continue
			}
			if len(p.Properties) > 0 {
				p.Properties = removeAnonymousProps(p.Properties)
			}
			newProps = append(newProps, p)
		}
		return newProps
	}
	for _, ps := range mt.PropertyStructs {
		ps.Properties = removeAnonymousProps(ps.Properties)
	}
}

// Squashes duplicates of the same property struct into single entries
func collapseDuplicatePropertyStructs(mt *ModuleType) {
	var collapsed []*PropertyStruct

propertyStructLoop:
	for _, from := range mt.PropertyStructs {
		for _, to := range collapsed {
			if from.Name == to.Name {
				CollapseDuplicateProperties(&to.Properties, &from.Properties)
				continue propertyStructLoop
			}
		}
		collapsed = append(collapsed, from)
	}
	mt.PropertyStructs = collapsed
}

func CollapseDuplicateProperties(to, from *[]Property) {
propertyLoop:
	for _, f := range *from {
		for i := range *to {
			t := &(*to)[i]
			if f.Name == t.Name {
				CollapseDuplicateProperties(&t.Properties, &f.Properties)
				continue propertyLoop
			}
		}
		*to = append(*to, f)
	}
}

// Find all property structs that only contain structs, and move their children up one with
// a prefixed name
func collapseNestedPropertyStructs(mt *ModuleType) {
	for _, ps := range mt.PropertyStructs {
		collapseNestedProperties(&ps.Properties)
	}
}

func collapseNestedProperties(p *[]Property) {
	var n []Property

	for _, parent := range *p {
		var containsProperty bool
		for j := range parent.Properties {
			child := &parent.Properties[j]
			if len(child.Properties) > 0 {
				collapseNestedProperties(&child.Properties)
			} else {
				containsProperty = true
			}
		}
		if containsProperty || len(parent.Properties) == 0 {
			n = append(n, parent)
		} else {
			for j := range parent.Properties {
				child := parent.Properties[j]
				child.Name = parent.Name + "." + child.Name
				n = append(n, child)
			}
		}
	}
	*p = n
}
