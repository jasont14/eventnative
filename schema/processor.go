package schema

import (
	"bufio"
	"bytes"
	"fmt"
	"github.com/jitsucom/eventnative/enrichment"
	"github.com/jitsucom/eventnative/events"
	"github.com/jitsucom/eventnative/logging"
	"github.com/jitsucom/eventnative/maputils"
	"github.com/jitsucom/eventnative/timestamp"
	"github.com/jitsucom/eventnative/typing"
	"io"
	"strings"
	"text/template"
	"time"
)

type Processor struct {
	flattener            *Flattener
	fieldMapper          Mapper
	typeCasts            map[string]typing.DataType
	tableNameExtractFunc TableNameExtractFunction
	tableNameExpression  string
	pkFields             map[string]bool
	enrichmentRules      []enrichment.Rule
}

func NewProcessor(tableNameFuncExpression string, mappings []string, mappingType FieldMappingType, primaryKeyFields map[string]bool,
	enrichmentRules []enrichment.Rule) (*Processor, error) {
	mapper, typeCasts, err := NewFieldMapper(mappingType, mappings)
	if err != nil {
		return nil, err
	}

	if typeCasts == nil {
		typeCasts = map[string]typing.DataType{}
	}

	tmpl, err := template.New("table name extract").
		Parse(tableNameFuncExpression)
	if err != nil {
		return nil, fmt.Errorf("Error parsing table name template %v", err)
	}

	tableNameExtractFunc := func(object map[string]interface{}) (string, error) {
		//we need time type of _timestamp field for extracting table name with date template
		ts, ok := object[timestamp.Key]
		if !ok {
			return "", fmt.Errorf("Error extracting table name: %s field doesn't exist", timestamp.Key)
		}
		t, err := time.Parse(timestamp.Layout, ts.(string))
		if err != nil {
			return "", fmt.Errorf("Error extracting table name: malformed %s field: %v", timestamp.Key, err)
		}

		object[timestamp.Key] = t
		var buf bytes.Buffer
		if err := tmpl.Execute(&buf, object); err != nil {
			return "", fmt.Errorf("Error executing %s template: %v", tableNameFuncExpression, err)
		}

		//revert type of _timestamp field
		object[timestamp.Key] = ts

		// format "<no value>" -> null
		formatted := strings.ReplaceAll(buf.String(), "<no value>", "null")
		// format "Abc dse" -> "abc_dse"
		reformatted := strings.ReplaceAll(formatted, " ", "_")
		return strings.ToLower(reformatted), nil
	}

	return &Processor{
		flattener:            NewFlattener(),
		fieldMapper:          mapper,
		typeCasts:            typeCasts,
		tableNameExtractFunc: tableNameExtractFunc,
		tableNameExpression:  tableNameFuncExpression,
		pkFields:             primaryKeyFields,
		enrichmentRules:      enrichmentRules,
	}, nil
}

//ProcessFact return table representation, processed flatten object
func (p *Processor) ProcessFact(fact map[string]interface{}) (*Table, events.Fact, error) {
	return p.processObject(fact)
}

//ProcessFilePayload process file payload lines divided with \n. Line by line where 1 line = 1 json
//Return array of processed objects per table like {"table1": []objects, "table2": []objects},
//All failed events are moved to separate collection for sending to fallback
func (p *Processor) ProcessFilePayload(fileName string, payload []byte, breakOnError bool, parseFunc func([]byte) (map[string]interface{}, error)) (map[string]*ProcessedFile, []*events.FailedFact, error) {
	var failedFacts []*events.FailedFact
	filePerTable := map[string]*ProcessedFile{}
	input := bytes.NewBuffer(payload)
	reader := bufio.NewReaderSize(input, 64*1024)
	line, readErr := reader.ReadBytes('\n')

	for readErr == nil {
		object, err := parseFunc(line)
		if err != nil {
			return nil, nil, err
		}

		table, processedObject, err := p.processObject(object)
		if err != nil {
			if breakOnError {
				return nil, nil, err
			} else {
				logging.Warnf("Unable to process object %s: %v. This line will be stored in fallback.", string(line), err)

				failedFacts = append(failedFacts, &events.FailedFact{
					//remove last byte (\n)
					Event:   line[:len(line)-1],
					Error:   err.Error(),
					EventId: events.ExtractEventId(object),
				})
			}
		}

		//don't process empty object
		if table.Exists() {
			f, ok := filePerTable[table.Name]
			if !ok {
				filePerTable[table.Name] = &ProcessedFile{FileName: fileName, DataSchema: table, payload: []map[string]interface{}{processedObject}}
			} else {
				f.DataSchema.Columns.Merge(table.Columns)
				f.payload = append(f.payload, processedObject)
			}
		}

		line, readErr = reader.ReadBytes('\n')
		if readErr != nil && readErr != io.EOF {
			return nil, nil, fmt.Errorf("Error reading line in [%s] file: %v", fileName, readErr)
		}
	}

	return filePerTable, failedFacts, nil
}

//ProcessObjects process source chunk payload objects
//Return array of processed objects per table like {"table1": []objects, "table2": []objects}
//If at least 1 error occurred - this method return it
func (p *Processor) ProcessObjects(objects []map[string]interface{}) (map[string]*ProcessedFile, error) {
	unitPerTable := map[string]*ProcessedFile{}

	for _, object := range objects {
		table, processedObject, err := p.processObject(object)
		if err != nil {
			return nil, err
		}

		//don't process empty object
		if !table.Exists() {
			continue
		}

		unit, ok := unitPerTable[table.Name]
		if !ok {
			unitPerTable[table.Name] = &ProcessedFile{DataSchema: table, payload: []map[string]interface{}{processedObject}}
		} else {
			unit.DataSchema.Columns.Merge(table.Columns)
			unit.payload = append(unit.payload, processedObject)
		}
	}

	return unitPerTable, nil
}

//ApplyDBTyping call ApplyDBTypingToObject to every object in input *ProcessedFile payload
//return err if can't convert any field to DB schema type
func (p *Processor) ApplyDBTyping(dbSchema *Table, pf *ProcessedFile) error {
	for _, object := range pf.payload {
		if err := p.ApplyDBTypingToObject(dbSchema, object); err != nil {
			return err
		}
	}

	return nil
}

//ApplyDBTypingToObject convert all object fields to DB schema types
//change input object
//return err if can't convert any field to DB schema type
func (p *Processor) ApplyDBTypingToObject(dbSchema *Table, object map[string]interface{}) error {
	for k, v := range object {
		column := dbSchema.Columns[k]
		converted, err := typing.Convert(column.GetType(), v)
		if err != nil {
			return fmt.Errorf("Error applying DB type [%s] to input [%s] field with [%v] value: %v", column.GetType(), k, v, err)
		}
		object[k] = converted
	}

	return nil
}

//Return table representation of object and flatten, mapped object
//1. copy map and don't change input object
//2. execute enrichment rules
//3. remove toDelete fields from object
//4. map object
//5. flatten object
//6. apply typecast
func (p *Processor) processObject(objectsss map[string]interface{}) (*Table, map[string]interface{}, error) {
	objectCopy := maputils.CopyMap(objectsss)
	for _, rule := range p.enrichmentRules {
		err := rule.Execute(objectCopy)
		if err != nil {
			return nil, nil, fmt.Errorf("Error executing enrichment rule: [%s]: %v", rule.Name(), err)
		}
	}

	mappedObject, err := p.fieldMapper.Map(objectCopy)
	if err != nil {
		return nil, nil, fmt.Errorf("Error mapping object: %v", err)
	}

	flatObject, err := p.flattener.FlattenObject(mappedObject)
	if err != nil {
		return nil, nil, err
	}

	tableName, err := p.tableNameExtractFunc(flatObject)
	if err != nil {
		return nil, nil, fmt.Errorf("Error extracting table name. Template: %s: %v", p.tableNameExpression, err)
	}
	if tableName == "" {
		return nil, nil, fmt.Errorf("Unknown table name. Template: %s", p.tableNameExpression)
	}

	table := &Table{Name: tableName, Columns: Columns{}, PKFields: p.pkFields}

	//apply typecast and define column types
	//mapping typecast overrides default typecast
	for k, v := range flatObject {
		//reformat from json.Number into int64 or float64 and put back
		v = typing.ReformatValue(v)
		flatObject[k] = v
		//value type
		resultColumnType, err := typing.TypeFromValue(v)
		if err != nil {
			return nil, nil, fmt.Errorf("Error getting type of field [%s]: %v", k, err)
		}

		//default typecast
		if defaultType, ok := typing.DefaultTypes[k]; ok {
			converted, err := typing.Convert(defaultType, v)
			if err != nil {
				return nil, nil, fmt.Errorf("Error default converting field [%s]: %v", k, err)
			}

			resultColumnType = defaultType
			flatObject[k] = converted
		}

		//mapping typecast
		if toType, ok := p.typeCasts[k]; ok {
			converted, err := typing.Convert(toType, v)
			if err != nil {
				strType, getStrErr := typing.StringFromType(toType)
				if getStrErr != nil {
					strType = getStrErr.Error()
				}
				return nil, nil, fmt.Errorf("Error converting field [%s] to [%s]: %v", k, strType, err)
			}

			resultColumnType = toType
			flatObject[k] = converted
		}

		table.Columns[k] = NewColumn(resultColumnType)
	}

	return table, flatObject, nil
}
