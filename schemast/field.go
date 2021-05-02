// Copyright 2019-present Facebook
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package schemast

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"

	"entgo.io/ent/schema/field"
	"github.com/go-openapi/inflect"
)

// Field converts a *field.Descriptor back into an *ast.CallExpr of the ent field package that can be used
// to construct it.
func Field(desc *field.Descriptor) (*ast.CallExpr, error) {
	switch t := desc.Info.Type; {
	case t.Numeric(), t == field.TypeString, t == field.TypeBool:
		return fromSimpleType(desc)
	case t == field.TypeEnum:
		return fromEnumType(desc)
	default:
		return nil, fmt.Errorf("schemast: unsupported type %s", t.ConstName())
	}
}

// AppendField adds a field to the returned values of the Fields method of type typeName.
func (c *Context) AppendField(typeName string, desc *field.Descriptor) error {
	stmt, err := c.fieldsReturnStmt(typeName)
	if err != nil {
		return err
	}
	newField, err := Field(desc)
	if err != nil {
		return err
	}
	returned := stmt.Results[0]
	switch r := returned.(type) {
	case *ast.Ident:
		if r.Name == "nil" {
			stmt.Results = []ast.Expr{
				newFieldSliceWith(newField),
			}
			return nil
		}
		return fmt.Errorf("schemast: unexpected ident. expected nil got %s", r.Name)
	case *ast.CompositeLit:
		r.Elts = append(r.Elts, newField)
		return nil
	default:
		return fmt.Errorf("schemast: unexpected AST component type %T", r)
	}
}

// RemoveField removes a field from the returned values of the Fields method of type typeName.
func (c *Context) RemoveField(typeName string, fieldName string) error {
	stmt, err := c.fieldsReturnStmt(typeName)
	if err != nil {
		return err
	}
	returned, ok := stmt.Results[0].(*ast.CompositeLit)
	if !ok {
		return fmt.Errorf("schemast: unexpected AST component type %T", stmt.Results[0])
	}
	for i, item := range returned.Elts {
		call, ok := item.(*ast.CallExpr)
		if !ok {
			return fmt.Errorf("schemast: expected return statement elements to be call expressions")
		}
		name, err := extractFieldName(call)
		if err != nil {
			return err
		}
		if name == fieldName {
			returned.Elts = append(returned.Elts[:i], returned.Elts[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("schemast: could not find field %q in type %q", fieldName, typeName)
}

func newFieldCall(desc *field.Descriptor) *builderCall {
	return &builderCall{
		curr: &ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   ast.NewIdent("field"),
				Sel: ast.NewIdent(fieldConstructor(desc)),
			},
			Args: []ast.Expr{
				strLit(desc.Name),
			},
		},
	}
}

func fromEnumType(desc *field.Descriptor) (*ast.CallExpr, error) {
	call, err := fromSimpleType(desc)
	if err != nil {
		return nil, err
	}
	modifier := "Values"
	for _, pair := range desc.Enums {
		if pair.N != pair.V {
			modifier = "NamedValues"
			break
		}
	}
	args := make([]ast.Expr, 0, len(desc.Enums))
	for _, pair := range desc.Enums {
		args = append(args, strLit(pair.N))
		if modifier == "NamedValues" {
			args = append(args, strLit(pair.V))
		}
	}
	builder := &builderCall{curr: call}
	builder.method(modifier, args...)
	return builder.curr, nil
}

func fromSimpleType(desc *field.Descriptor) (*ast.CallExpr, error) {
	builder := newFieldCall(desc)
	if desc.Nillable {
		builder.method("Nillable")
	}
	if desc.Optional {
		builder.method("Optional")
	}
	if desc.Unique {
		builder.method("Unique")
	}
	if desc.Sensitive {
		builder.method("Sensitive")
	}
	if desc.Immutable {
		builder.method("Immutable")
	}
	if desc.Comment != "" {
		builder.method("Comment", strLit(desc.Comment))
	}
	if desc.Tag != "" {
		builder.method("StructTag", strLit(desc.Tag))
	}
	if desc.StorageKey != "" {
		builder.method("StorageKey", strLit(desc.StorageKey))
	}
	if len(desc.SchemaType) > 0 {
		builder.method("SchemaType", strMapLit(desc.SchemaType))
	}

	// Unsupported features
	var unsupported error
	if len(desc.Annotations) != 0 {
		unsupported = combineUnsupported(unsupported, "Descriptor.Annotations")
	}
	if len(desc.Validators) != 0 {
		unsupported = combineUnsupported(unsupported, "Descriptor.Validators")
	}
	if desc.Default != nil {
		unsupported = combineUnsupported(unsupported, "Descriptor.Default")
	}
	if desc.UpdateDefault != nil {
		unsupported = combineUnsupported(unsupported, "Descriptor.UpdateDefault")
	}
	if unsupported != nil {
		return nil, unsupported
	}
	return builder.curr, nil
}

func fieldConstructor(dsc *field.Descriptor) string {
	return strings.TrimPrefix(dsc.Info.ConstName(), "Type")
}

func (c *Context) fieldsReturnStmt(typeName string) (*ast.ReturnStmt, error) {
	fd, err := c.lookupMethod(typeName, "Fields")
	if err != nil {
		return nil, err
	}
	if len(fd.Body.List) != 1 {
		return nil, fmt.Errorf("schmeast: Fields() func body must have a single element")
	}
	if _, ok := fd.Body.List[0].(*ast.ReturnStmt); !ok {
		return nil, fmt.Errorf("schmeast: Fields() func body must contain a return statement")
	}
	return fd.Body.List[0].(*ast.ReturnStmt), err
}

func newFieldSliceWith(f *ast.CallExpr) *ast.CompositeLit {
	return &ast.CompositeLit{
		Type: &ast.ArrayType{
			Elt: &ast.SelectorExpr{
				X:   ast.NewIdent("ent"),
				Sel: ast.NewIdent("Field"),
			},
		},
		Elts: []ast.Expr{
			f,
		},
	}
}

func extractFieldName(fd *ast.CallExpr) (string, error) {
	sel, ok := fd.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", fmt.Errorf("schemast: unexpected type %T", fd.Fun)
	}
	if inner, ok := sel.X.(*ast.CallExpr); ok {
		return extractFieldName(inner)
	}
	if final, ok := sel.X.(*ast.Ident); ok && final.Name != "field" {
		return "", fmt.Errorf(`schemast: expected field AST to be of form field.<Type>("name")`)
	}
	if len(fd.Args) == 0 {
		return "", fmt.Errorf("schemast: expected field constructor to have at least name arg")
	}
	name, ok := fd.Args[0].(*ast.BasicLit)
	if !ok && name.Kind == token.STRING {
		return "", fmt.Errorf("schemast: expected field name to be a string literal")
	}
	return strconv.Unquote(name.Value)
}

func (c *Context) AddType(typeName string) error {
	body := fmt.Sprintf(`package schema
import "entgo.io/ent"
type %s struct {
	ent.Schema
}
func (%s) Fields() []ent.Field {
	return nil
}
func (%s) Edges() []ent.Edge {
	return nil
}
`, typeName, typeName, typeName)
	fn := inflect.Underscore(typeName) + ".go"
	f, err := parser.ParseFile(c.SchemaPackage.Fset, fn, body, 0)
	if err != nil {
		return err
	}
	c.newTypes[typeName] = f
	return nil
}