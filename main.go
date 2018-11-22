// protoc plugin which converts .proto to JSON schema
// It is spawned by protoc and generates JSON-schema files.
// "Heavily influenced" by Google's "protog-gen-bq-schema"
//
// usage:
//  $ bin/protoc --jsonschema_out=path/to/outdir foo.proto
//
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"strings"

	"github.com/alecthomas/jsonschema"
	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/protoc-gen-go/descriptor"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
	"github.com/xeipuuv/gojsonschema"
)

const (
	LOG_DEBUG = 0
	LOG_INFO  = 1
	LOG_WARN  = 2
	LOG_ERROR = 3
	LOG_FATAL = 4
	LOG_PANIC = 5
)

var (
	allowNullValues              bool = false
	disallowAdditionalProperties bool = false
	disallowBigIntsAsStrings     bool = false
	debugLogging                 bool = false
	globalPkg                         = &ProtoPackage{
		name:     "",
		parent:   nil,
		children: make(map[string]*ProtoPackage),
		types:    make(map[string]*descriptor.DescriptorProto),
	}
	logLevels = map[LogLevel]string{
		0: "DEBUG",
		1: "INFO",
		2: "WARN",
		3: "ERROR",
		4: "FATAL",
		5: "PANIC",
	}
)

// ProtoPackage describes a package of Protobuf, which is an container of message types.
type ProtoPackage struct {
	name     string
	parent   *ProtoPackage
	children map[string]*ProtoPackage
	types    map[string]*descriptor.DescriptorProto
}

//ProtoDescription ...
type ProtoDescription struct {
	Package  string                                        `json:"package"`
	Enums    map[string]jsonschema.Type                   `json:"enums,omitempty"`
	Messages map[string]jsonschema.Type                   `json:"messages,omitempty"`
	Services map[string]*descriptor.ServiceDescriptorProto `json:"services,omitempty"`
}

type LogLevel int

func init() {
	flag.BoolVar(&allowNullValues, "allow_null_values", false, "Allow NULL values to be validated")
	flag.BoolVar(&disallowAdditionalProperties, "disallow_additional_properties", false, "Disallow additional properties")
	flag.BoolVar(&disallowBigIntsAsStrings, "disallow_bigints_as_strings", true, "Disallow bigints to be strings (eg scientific notation)")
	flag.BoolVar(&debugLogging, "debug", true, "Log debug messages")
}

func logWithLevel(logLevel LogLevel, logFormat string, logParams ...interface{}) {
	// If we're not doing debug logging then just return:
	if logLevel <= LOG_INFO && !debugLogging {
		return
	}

	// Otherwise log:
	logMessage := fmt.Sprintf(logFormat, logParams...)
	log.Printf(fmt.Sprintf("[%v] %v", logLevels[logLevel], logMessage))
}

func registerType(pkgName *string, msg *descriptor.DescriptorProto) {
	pkg := globalPkg
	if pkgName != nil {
		for _, node := range strings.Split(*pkgName, ".") {
			if pkg == globalPkg && node == "" {
				// Skips leading "."
				continue
			}
			child, ok := pkg.children[node]
			if !ok {
				child = &ProtoPackage{
					name:     pkg.name + "." + node,
					parent:   pkg,
					children: make(map[string]*ProtoPackage),
					types:    make(map[string]*descriptor.DescriptorProto),
				}
				pkg.children[node] = child
			}
			pkg = child
		}
	}
	pkg.types[msg.GetName()] = msg
}

func (pkg *ProtoPackage) lookupType(name string) (*descriptor.DescriptorProto, bool) {
	if strings.HasPrefix(name, ".") {
		return globalPkg.relativelyLookupType(name[1:len(name)])
	}

	for ; pkg != nil; pkg = pkg.parent {
		if desc, ok := pkg.relativelyLookupType(name); ok {
			return desc, ok
		}
	}
	return nil, false
}

func relativelyLookupNestedType(desc *descriptor.DescriptorProto, name string) (*descriptor.DescriptorProto, bool) {
	components := strings.Split(name, ".")
componentLoop:
	for _, component := range components {
		for _, nested := range desc.GetNestedType() {
			if nested.GetName() == component {
				desc = nested
				continue componentLoop
			}
		}
		logWithLevel(LOG_INFO, "no such nested message %s in %s", component, desc.GetName())
		return nil, false
	}
	return desc, true
}

func (pkg *ProtoPackage) relativelyLookupType(name string) (*descriptor.DescriptorProto, bool) {
	components := strings.SplitN(name, ".", 2)
	switch len(components) {
	case 0:
		logWithLevel(LOG_DEBUG, "empty message name")
		return nil, false
	case 1:
		found, ok := pkg.types[components[0]]
		return found, ok
	case 2:
		logWithLevel(LOG_DEBUG, "looking for %s in %s at %s (%v)", components[1], components[0], pkg.name, pkg)
		if child, ok := pkg.children[components[0]]; ok {
			found, ok := child.relativelyLookupType(components[1])
			return found, ok
		}
		if msg, ok := pkg.types[components[0]]; ok {
			found, ok := relativelyLookupNestedType(msg, components[1])
			return found, ok
		}
		logWithLevel(LOG_INFO, "no such package nor message %s in %s", components[0], pkg.name)
		return nil, false
	default:
		logWithLevel(LOG_FATAL, "not reached")
		return nil, false
	}
}

func (pkg *ProtoPackage) relativelyLookupPackage(name string) (*ProtoPackage, bool) {
	components := strings.Split(name, ".")
	for _, c := range components {
		var ok bool
		pkg, ok = pkg.children[c]
		if !ok {
			return nil, false
		}
	}
	return pkg, true
}

// Convert a proto "field" (essentially a type-switch with some recursion):
func convertField(curPkg *ProtoPackage, desc *descriptor.FieldDescriptorProto, msg *descriptor.DescriptorProto) (*jsonschema.Type, error) {

	// Prepare a new jsonschema.Type for our eventual return value:
	jsonSchemaType := &jsonschema.Type{
		Properties: make(map[string]*jsonschema.Type),
		Title:      desc.GetName(),
	}

	// Switch the types, and pick a JSONSchema equivalent:
	switch desc.GetType() {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE,
		descriptor.FieldDescriptorProto_TYPE_FLOAT:
		if allowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_NUMBER},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_NUMBER
		}

	case descriptor.FieldDescriptorProto_TYPE_INT32,
		descriptor.FieldDescriptorProto_TYPE_UINT32,
		descriptor.FieldDescriptorProto_TYPE_FIXED32,
		descriptor.FieldDescriptorProto_TYPE_SFIXED32,
		descriptor.FieldDescriptorProto_TYPE_SINT32:
		if allowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_INTEGER},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
		}

	case descriptor.FieldDescriptorProto_TYPE_INT64,
		descriptor.FieldDescriptorProto_TYPE_UINT64,
		descriptor.FieldDescriptorProto_TYPE_FIXED64,
		descriptor.FieldDescriptorProto_TYPE_SFIXED64,
		descriptor.FieldDescriptorProto_TYPE_SINT64:
		jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
		//jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: gojsonschema.TYPE_INTEGER})
		if !disallowBigIntsAsStrings {
			jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: gojsonschema.TYPE_STRING})
		}
		if allowNullValues {
			jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: gojsonschema.TYPE_NULL})
		}

	case descriptor.FieldDescriptorProto_TYPE_STRING,
		descriptor.FieldDescriptorProto_TYPE_BYTES:
		if allowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_STRING},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_STRING
		}

	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		//jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: gojsonschema.TYPE_STRING})
		//jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: gojsonschema.TYPE_INTEGER})
		jsonSchemaType.Type = gojsonschema.TYPE_INTEGER
		//添加引用来源
		jsonSchemaType.Ref = desc.GetTypeName()[1:]

		if allowNullValues {
			jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: gojsonschema.TYPE_NULL})
		}

		// Go through all the enums we have, see if we can match any to this field by name:
		for _, enumDescriptor := range msg.GetEnumType() {

			// Each one has several values:
			for _, enumValue := range enumDescriptor.Value {
				/* 所有的枚举类型都展开
				// Figure out the entire name of this field:
				fullFieldName := fmt.Sprintf(".%v.%v", *msg.Name, *enumDescriptor.Name)

				// If we find ENUM values for this field then put them into the JSONSchema list of allowed ENUM values:
				if strings.HasSuffix(desc.GetTypeName(), fullFieldName) {
					jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Name)
					jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Number)
				}
				*/

				jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Name)
				jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Number)
			}
		}

		//定义在message 外的enum，也展开
		if len(msg.GetEnumType()) == 0 {
			recordType, ok := curPkg.lookupType(desc.GetTypeName()[1:])
			if !ok {
				return nil, fmt.Errorf("no such message type named %s", desc.GetTypeName())
			}
			for _, enumDescriptor := range recordType.GetEnumType() {
				// Each one has several values:
				for _, enumValue := range enumDescriptor.Value {
					jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Name)
					jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Number)
				}
			}
		}

	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		if allowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_BOOLEAN},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_BOOLEAN
		}

	case descriptor.FieldDescriptorProto_TYPE_GROUP,
		descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
		//去掉前面的. 添加引用
		jsonSchemaType.Ref = desc.GetTypeName()[1:]

		if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_OPTIONAL {
			jsonSchemaType.AdditionalProperties = []byte("true")
		}
		if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REQUIRED {
			jsonSchemaType.AdditionalProperties = []byte("false")
		}

	default:
		return nil, fmt.Errorf("unrecognized field type: %s", desc.GetType().String())
	}

	// Recurse array of primitive types:
	if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REPEATED && jsonSchemaType.Type != gojsonschema.TYPE_OBJECT {
		jsonSchemaType.Items = &jsonschema.Type{
			Type:  jsonSchemaType.Type,
			OneOf: jsonSchemaType.OneOf,
		}
		if allowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: gojsonschema.TYPE_ARRAY},
			}
		} else {
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY
			jsonSchemaType.OneOf = []*jsonschema.Type{}
		}

		return jsonSchemaType, nil
	}

	// Recurse nested objects / arrays of objects (if necessary):
	if jsonSchemaType.Type == gojsonschema.TYPE_OBJECT {

		recordType, ok := curPkg.lookupType(desc.GetTypeName())
		if !ok {
			return nil, fmt.Errorf("no such message type named %s", desc.GetTypeName())
		}

		// Recurse:
		recursedJSONSchemaType, err := convertMessageType(curPkg, recordType)
		if err != nil {
			return nil, err
		}

		// The result is stored differently for arrays of objects (they become "items"):
		if desc.GetLabel() == descriptor.FieldDescriptorProto_LABEL_REPEATED {
			jsonSchemaType.Items = &recursedJSONSchemaType
			jsonSchemaType.Type = gojsonschema.TYPE_ARRAY
		} else {
			// Nested objects are more straight-forward:
			jsonSchemaType.Properties = recursedJSONSchemaType.Properties
		}

		// Optionally allow NULL values:
		if allowNullValues {
			jsonSchemaType.OneOf = []*jsonschema.Type{
				{Type: gojsonschema.TYPE_NULL},
				{Type: jsonSchemaType.Type},
			}
			jsonSchemaType.Type = ""
		}
	}

	return jsonSchemaType, nil
}

// Converts a proto "MESSAGE" into a JSON-Schema:
func convertMessageType(curPkg *ProtoPackage, msg *descriptor.DescriptorProto) (jsonschema.Type, error) {

	// Prepare a new jsonschema:
	jsonSchemaType := jsonschema.Type{
		Properties: make(map[string]*jsonschema.Type),
		Version:    jsonschema.Version,
		//去掉前面的.
		Description: curPkg.name[1:] + "." + msg.GetName(),
		Title:       msg.GetName(),
	}

	// Optionally allow NULL values:
	if allowNullValues {
		jsonSchemaType.OneOf = []*jsonschema.Type{
			{Type: gojsonschema.TYPE_NULL},
			{Type: gojsonschema.TYPE_OBJECT},
		}
	} else {
		jsonSchemaType.Type = gojsonschema.TYPE_OBJECT
	}

	// disallowAdditionalProperties will prevent validation where extra fields are found (outside of the schema):
	if disallowAdditionalProperties {
		jsonSchemaType.AdditionalProperties = []byte("false")
	} else {
		jsonSchemaType.AdditionalProperties = []byte("true")
	}

	logWithLevel(LOG_DEBUG, "Converting message: %s", proto.MarshalTextString(msg))
	for _, fieldDesc := range msg.GetField() {
		recursedJSONSchemaType, err := convertField(curPkg, fieldDesc, msg)
		if err != nil {
			logWithLevel(LOG_ERROR, "Failed to convert field %s in %s: %v", fieldDesc.GetName(), msg.GetName(), err)
			return jsonSchemaType, err
		}
		jsonSchemaType.Properties[fieldDesc.GetName()] = recursedJSONSchemaType
	}
	return jsonSchemaType, nil
}

// Converts a proto "ENUM" into a JSON-Schema:
func convertEnumType(curPkg *ProtoPackage, enum *descriptor.EnumDescriptorProto) (jsonschema.Type, error) {

	// Prepare a new jsonschema.Type for our eventual return value:
	jsonSchemaType := jsonschema.Type{
		Version:     jsonschema.Version,
		Description: curPkg.name[1:] + "." + enum.GetName(),
	}

	// Allow both strings and integers:
	//jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: "string"})
	//jsonSchemaType.OneOf = append(jsonSchemaType.OneOf, &jsonschema.Type{Type: "integer"})
	jsonSchemaType.Type = gojsonschema.TYPE_INTEGER

	// Add the allowed values:
	for _, enumValue := range enum.Value {
		jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Name)
		jsonSchemaType.Enum = append(jsonSchemaType.Enum, enumValue.Number)
	}

	return jsonSchemaType, nil
}

func convertServiceType(curPkg *ProtoPackage, svr *descriptor.ServiceDescriptorProto) (*descriptor.ServiceDescriptorProto, error) {
	newSvr := descriptor.ServiceDescriptorProto{
		Name: svr.Name,
		Options: svr.Options,
		Method: make([]*descriptor.MethodDescriptorProto, len(svr.Method)),
	}
	for i := range svr.Method {
		inputType := ""
		if svr.Method[i].InputType != nil {
			//去掉前面的.
			inputType = (*svr.Method[i].InputType)[1:]
		}
		outputType := ""
		if svr.Method[i].OutputType != nil {
			//去掉前面的.
			outputType = (*svr.Method[i].OutputType)[1:]
		}
		newSvr.Method[i] = &descriptor.MethodDescriptorProto{
			Name:   svr.Method[i].Name,
			InputType: &inputType,
			OutputType: &outputType,
			Options: svr.Method[i].Options,
			ClientStreaming: svr.Method[i].ClientStreaming,
			ServerStreaming: svr.Method[i].ServerStreaming,
		}
	}

	return &newSvr, nil
}

// Converts a proto file into a JSON-Schema:
func convertFile(file *descriptor.FileDescriptorProto) ([]*plugin.CodeGeneratorResponse_File, error) {

	// Input filename:
	protoFileName := path.Base(file.GetName())

	// Prepare a list of responses:
	response := []*plugin.CodeGeneratorResponse_File{}

	// Warn about multiple messages / enums in files:
	if len(file.GetMessageType()) > 1 {
		logWithLevel(LOG_WARN, "protoc-gen-jsonschema will create multiple MESSAGE schemas (%d) from one proto file (%v)", len(file.GetMessageType()), protoFileName)
	}
	if len(file.GetEnumType()) > 1 {
		logWithLevel(LOG_WARN, "protoc-gen-jsonschema will create multiple ENUM schemas (%d) from one proto file (%v)", len(file.GetEnumType()), protoFileName)
	}

	pkg, ok := globalPkg.relativelyLookupPackage(file.GetPackage())
	if !ok {
		return nil, fmt.Errorf("no such package found: %s", file.GetPackage())
	}

	protoDesc := ProtoDescription{
		Package: file.GetPackage(),
		Enums:  make(map[string]jsonschema.Type, 0),
		Messages: make(map[string]jsonschema.Type, 0),
		Services: make(map[string]*descriptor.ServiceDescriptorProto, 0),
	}

	// Generate ENUMs:
	for _, enum := range file.GetEnumType() {
		//jsonSchemaFileName := fmt.Sprintf("%s.enum.schema.json", file.GetPackage()+"."+enum.GetName())
		//logWithLevel(LOG_INFO, "Generating JSON-schema for ENUM (%v) in file [%v] => %v", enum.GetName(), protoFileName, jsonSchemaFileName)
		logWithLevel(LOG_INFO, "Generating JSON-schema for ENUM (%v) => %v", enum.GetName(), protoFileName)
		enumJsonSchema, err := convertEnumType(pkg, enum)
		if err != nil {
			logWithLevel(LOG_ERROR, "Failed to convert %s: %v", protoFileName, err)
			return nil, err
		}
		protoDesc.Enums[enum.GetName()] = enumJsonSchema
		// Marshal the JSON-Schema into JSON:
		//jsonSchemaJSON, err := json.MarshalIndent(enumJsonSchema, "", "    ")
		//if err != nil {
		//	logWithLevel(LOG_ERROR, "Failed to encode jsonSchema: %v", err)
		//	return nil, err
		//}
		//// Add a response:
		//resFile := &plugin.CodeGeneratorResponse_File{
		//	Name:    proto.String(jsonSchemaFileName),
		//	Content: proto.String(string(jsonSchemaJSON)),
		//}
		//response = append(response, resFile)
	}

	//Generate message json schema
	for _, msg := range file.GetMessageType() {
		//jsonSchemaFileName := fmt.Sprintf("%s.msg.schema.json", file.GetPackage()+"."+msg.GetName())
		//logWithLevel(LOG_INFO, "Generating JSON-schema for MESSAGE (%v) in file [%v] => %v", msg.GetName(), protoFileName, jsonSchemaFileName)
		logWithLevel(LOG_INFO, "Generating JSON-schema for MESSAGE (%v) => %v", msg.GetName(), protoFileName)
		messageJSONSchema, err := convertMessageType(pkg, msg)
		if err != nil {
			logWithLevel(LOG_ERROR, "Failed to convert %s: %v", protoFileName, err)
			return nil, err
		}
		protoDesc.Messages[msg.GetName()] = messageJSONSchema
		//// Marshal the JSON-Schema into JSON:
		//jsonSchemaJSON, err := json.MarshalIndent(messageJSONSchema, "", "    ")
		//if err != nil {
		//	logWithLevel(LOG_ERROR, "Failed to encode jsonSchema: %v", err)
		//	return nil, err
		//}
		//// Add a response:
		//resFile := &plugin.CodeGeneratorResponse_File{
		//	Name:    proto.String(jsonSchemaFileName),
		//	Content: proto.String(string(jsonSchemaJSON)),
		//}
		//response = append(response, resFile)
	}

	//Generate service json schema
	for _, svr := range file.GetService() {
		//jsonSchemaFileName := fmt.Sprintf("%s.srv.schema.json", file.GetPackage()+"."+svr.GetName())
		//logWithLevel(LOG_INFO, "Generating JSON-schema for SERVICE (%v) in file [%v] => %v", svr.GetName(), protoFileName, jsonSchemaFileName)
		serviceJSONSchema, err := convertServiceType(pkg, svr)
		if err != nil {
			logWithLevel(LOG_ERROR, "Failed to convert %s: %v", protoFileName, err)
			return nil, err
		}
		protoDesc.Services[svr.GetName()] = serviceJSONSchema

		//jsonSchemaJSON, err := json.MarshalIndent(serviceJSONSchema, "", "    ")
		//if err != nil {
		//	logWithLevel(LOG_ERROR, "Failed to encode jsonSchema: %v", err)
		//	return nil, err
		//}
		//resFile := &plugin.CodeGeneratorResponse_File{
		//	Name:    proto.String(jsonSchemaFileName),
		//	Content: proto.String(string(jsonSchemaJSON)),
		//}
		//response = append(response, resFile)
	}
	jsonSchemaJSON, err := json.MarshalIndent(protoDesc, "", "    ")
	if err != nil {
		logWithLevel(LOG_ERROR, "Failed to encode jsonSchema: %v", err)
		return nil, err
	}
	resFile := &plugin.CodeGeneratorResponse_File {
		Name: proto.String(protoFileName + ".schema.json"),
		Content: proto.String(string(jsonSchemaJSON)),
	}
	response = append(response, resFile)
	return response, nil
}

func convert(req *plugin.CodeGeneratorRequest) (*plugin.CodeGeneratorResponse, error) {
	generateTargets := make(map[string]bool)
	for _, file := range req.GetFileToGenerate() {
		generateTargets[file] = true
	}

	res := &plugin.CodeGeneratorResponse{}
	for _, file := range req.GetProtoFile() {
		for _, msg := range file.GetMessageType() {
			logWithLevel(LOG_DEBUG, "Loading a message type %s from package %s", msg.GetName(), file.GetPackage())
			registerType(file.Package, msg)
		}
		//enum也记录保存下来
		for _, enum := range file.GetEnumType() {
			msg := &descriptor.DescriptorProto{
				Name:     enum.Name,
				EnumType: []*descriptor.EnumDescriptorProto{enum,},
			}
			logWithLevel(LOG_DEBUG, "Loading a message type %s from package %s", msg.GetName(), file.GetPackage())
			registerType(file.Package, msg)
		}
	}
	for _, file := range req.GetProtoFile() {
		if _, ok := generateTargets[file.GetName()]; ok {
			logWithLevel(LOG_DEBUG, "Converting file (%v)", file.GetName())
			converted, err := convertFile(file)
			if err != nil {
				res.Error = proto.String(fmt.Sprintf("Failed to convert %s: %v", file.GetName(), err))
				return res, err
			}
			res.File = append(res.File, converted...)
		}
	}
	return res, nil
}

func convertFrom(rd io.Reader) (*plugin.CodeGeneratorResponse, error) {
	logWithLevel(LOG_DEBUG, "Reading code generation request")
	input, err := ioutil.ReadAll(rd)
	if err != nil {
		logWithLevel(LOG_ERROR, "Failed to read request: %v", err)
		return nil, err
	}

	req := &plugin.CodeGeneratorRequest{}
	err = proto.Unmarshal(input, req)
	if err != nil {
		logWithLevel(LOG_ERROR, "Can't unmarshal input: %v", err)
		return nil, err
	}

	commandLineParameter(req.GetParameter())

	logWithLevel(LOG_DEBUG, "Converting input")
	return convert(req)
}

func commandLineParameter(parameters string) {
	for _, parameter := range strings.Split(parameters, ",") {
		switch parameter {
		case "allow_null_values":
			allowNullValues = true
		case "debug":
			debugLogging = true
		case "disallow_additional_properties":
			disallowAdditionalProperties = true
		case "disallow_bigints_as_strings":
			disallowBigIntsAsStrings = true
		}
	}
}

func main() {
	flag.Parse()
	ok := true
	logWithLevel(LOG_DEBUG, "Processing code generator request")
	res, err := convertFrom(os.Stdin)
	if err != nil {
		ok = false
		if res == nil {
			message := fmt.Sprintf("Failed to read input: %v", err)
			res = &plugin.CodeGeneratorResponse{
				Error: &message,
			}
		}
	}

	logWithLevel(LOG_DEBUG, "Serializing code generator response")
	data, err := proto.Marshal(res)
	if err != nil {
		logWithLevel(LOG_FATAL, "Cannot marshal response: %v", err)
	}
	_, err = os.Stdout.Write(data)
	if err != nil {
		logWithLevel(LOG_FATAL, "Failed to write response: %v", err)
	}

	if ok {
		logWithLevel(LOG_DEBUG, "Succeeded to process code generator request")
	} else {
		logWithLevel(LOG_WARN, "Failed to process code generator but successfully sent the error to protoc")
		os.Exit(1)
	}
}
