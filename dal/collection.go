package dal

import (
	"fmt"
	"github.com/fatih/structs"
	"github.com/ghetzel/go-stockutil/sliceutil"
	"reflect"
)

type CollectionAction int

const (
	SchemaVerify CollectionAction = iota
	SchemaCreate
	SchemaExpand
	SchemaRemove
	SchemaEnforce
)

var DefaultIdentityField = `id`
var DefaultIdentityFieldType Type = IntType

type CollectionOptions struct {
	FieldsUnordered bool `json:"fields_unordered,omitempty"`
}

type Collection struct {
	Name              string             `json:"name"`
	Fields            []Field            `json:"fields"`
	IdentityField     string             `json:"identity_field,omitempty"`
	IdentityFieldType Type               `json:"identity_field_type,omitempty"`
	Options           *CollectionOptions `json:"options,omitempty"`
	recordType        reflect.Type
}

func NewCollection(name string) *Collection {
	return &Collection{
		Name:              name,
		Fields:            make([]Field, 0),
		IdentityField:     DefaultIdentityField,
		IdentityFieldType: DefaultIdentityFieldType,
	}
}

func (self *Collection) AddFields(fields ...Field) *Collection {
	self.Fields = append(self.Fields, fields...)
	return self
}

// func (self *Collection) SetRecordType(in interface{}) *Collection {
// 	inT := reflect.TypeOf(in)

// 	switch inT.Kind() {
// 	case reflect.Struct, reflect.Map:
// 		self.recordType = inT
// 	default:
// 		fallbackType := make(map[string]interface{})
// 		self.recordType = reflect.TypeOf(fallbackType)
// 	}

// 	return self
// }

func (self *Collection) GetField(name string) (Field, bool) {
	for _, field := range self.Fields {
		if field.Name == name {
			return field, true
		}
	}

	return Field{}, false
}

func (self *Collection) ConvertValue(name string, value interface{}) (interface{}, error) {
	if field, ok := self.GetField(name); ok {
		return field.ConvertValue(value)
	} else {
		return nil, fmt.Errorf("Unknown field '%s'", name)
	}
}

func (self *Collection) MakeRecord(in interface{}) (*Record, error) {
	if err := validatePtrToStructType(in); err != nil {
		return nil, err
	}

	// create the record we're going to populate
	record := NewRecord(nil)
	s := structs.New(in)

	// get a string slice of the field names that are valid for this collection
	actualFieldNames := make([]string, 0)

	for _, field := range self.Fields {
		actualFieldNames = append(actualFieldNames, field.Name)
	}

	// get details for the fields present on the given input struct
	if fields, err := getFieldsForStruct(s); err == nil {
		// for each field descriptor...
		for tagName, fieldDescr := range fields {
			value := fieldDescr.Field.Value()

			// if we're supposed to skip empty values, and this value is indeed empty, skip
			if fieldDescr.OmitEmpty && value == reflect.Zero(reflect.TypeOf(value)).Interface() {
				continue
			}

			// set the ID field if this field is explicitly marked as the identity
			if fieldDescr.Identity {
				record.ID = value
			} else if sliceutil.ContainsString(actualFieldNames, tagName) {
				record.Set(tagName, value)
			}
		}

		// an identity column was not explicitly specified, so try to find the column that matches
		// our identity field name
		if record.ID == nil {
			for tagName, fieldDescr := range fields {
				if tagName == self.IdentityField {
					record.ID = fieldDescr.Field.Value()
					delete(record.Fields, tagName)
					break
				}
			}
		}

		// an ID still wasn't found, so try the field called "ID"
		if record.ID == nil {
			if field, ok := s.FieldOk(`ID`); ok {
				record.ID = field.Value()
				delete(record.Fields, `ID`)
			}
		}

		return record, nil
	} else {
		return nil, err
	}
}
