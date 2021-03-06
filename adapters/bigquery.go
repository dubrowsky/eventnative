package adapters

import (
	"cloud.google.com/go/bigquery"
	"context"
	"fmt"
	"github.com/ksensehq/eventnative/schema"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"log"
	"net/http"
	"strings"
)

var (
	SchemaToBigQuery = map[schema.DataType]bigquery.FieldType{
		schema.STRING: bigquery.StringFieldType,
	}

	BigQueryToSchema = map[bigquery.FieldType]schema.DataType{
		bigquery.StringFieldType: schema.STRING,
	}
)

type BigQuery struct {
	ctx    context.Context
	client *bigquery.Client
	config *GoogleConfig
}

func NewBigQuery(ctx context.Context, config *GoogleConfig) (*BigQuery, error) {
	credentials := extractCredentials(config)
	client, err := bigquery.NewClient(ctx, config.Project, credentials)
	if err != nil {
		return nil, fmt.Errorf("Error creating BigQuery client: %v", err)
	}

	return &BigQuery{ctx: ctx, client: client, config: config}, nil
}

//Transfer data from google cloud storage file to google BigQuery table
//as one batch
func (bq *BigQuery) Copy(fileKey, tableName string) error {
	table := bq.client.Dataset(bq.config.Dataset).Table(tableName)

	gcsRef := bigquery.NewGCSReference(fmt.Sprintf("gs://%s/%s", bq.config.Bucket, fileKey))
	gcsRef.SourceFormat = bigquery.JSON
	loader := table.LoaderFrom(gcsRef)
	loader.CreateDisposition = bigquery.CreateNever

	job, err := loader.Run(bq.ctx)
	if err != nil {
		return fmt.Errorf("Error running loading from google cloud storage to BigQuery table %s: %v", tableName, err)
	}
	jobStatus, err := job.Wait(bq.ctx)
	if err != nil {
		return fmt.Errorf("Error waiting loading job from google cloud storage to BigQuery table %s: %v", tableName, err)
	}

	if jobStatus.Err() != nil {
		return fmt.Errorf("Error loading from google cloud storage to BigQuery table %s: %v", tableName, err)
	}

	return nil
}

//Return google BigQuery table representation(name, columns with types) as schema.Table
func (bq *BigQuery) GetTableSchema(tableName string) (*schema.Table, error) {
	table := &schema.Table{Name: tableName, Columns: schema.Columns{}}

	bqTable := bq.client.Dataset(bq.config.Dataset).Table(tableName)

	meta, err := bqTable.Metadata(bq.ctx)
	if err != nil {
		if isNotFoundErr(err) {
			return table, nil
		}

		return nil, fmt.Errorf("Error querying BigQuery table [%s] metadata: %v", tableName, err)
	}

	for _, field := range meta.Schema {
		mappedType, ok := BigQueryToSchema[field.Type]
		if !ok {
			log.Println("Unknown BigQuery column type:", field.Type)
			mappedType = schema.STRING
		}
		table.Columns[field.Name] = schema.Column{Type: mappedType}
	}

	return table, nil
}

//Create google BigQuery table from schema.Table
func (bq *BigQuery) CreateTable(tableSchema *schema.Table) error {
	bqTable := bq.client.Dataset(bq.config.Dataset).Table(tableSchema.Name)

	_, err := bqTable.Metadata(bq.ctx)
	if err == nil {
		log.Println("BigQuery table", tableSchema.Name, "already exists")
		return nil
	}

	if !isNotFoundErr(err) {
		return fmt.Errorf("Error getting new table %s metadata: %v", tableSchema.Name, err)
	}

	bqSchema := bigquery.Schema{}
	for columnName, column := range tableSchema.Columns {
		mappedType, ok := SchemaToBigQuery[column.Type]
		if !ok {
			log.Println("Unknown BigQuery schema type:", column.Type)
			mappedType = SchemaToBigQuery[schema.STRING]
		}
		bqSchema = append(bqSchema, &bigquery.FieldSchema{Name: columnName, Type: mappedType})
	}

	if err := bqTable.Create(bq.ctx, &bigquery.TableMetadata{Name: tableSchema.Name, Schema: bqSchema}); err != nil {
		return fmt.Errorf("Error creating [%s] BigQuery table %v", tableSchema.Name, err)
	}

	return nil
}

//Create google BigQuery Dataset if doesn't exist
func (bq *BigQuery) CreateDataset(dataset string) error {
	bqDataset := bq.client.Dataset(dataset)
	if _, err := bqDataset.Metadata(bq.ctx); err != nil {
		if isNotFoundErr(err) {
			if err := bqDataset.Create(bq.ctx, &bigquery.DatasetMetadata{Name: dataset}); err != nil {
				return fmt.Errorf("Error creating dataset %s in BigQuery: %v", dataset, err)
			}
		} else {
			return fmt.Errorf("Error getting dataset %s in BigQuery: %v", dataset, err)
		}
	}

	return nil
}

//Add schema.Table columns to google BigQuery table
func (bq *BigQuery) PatchTableSchema(patchSchema *schema.Table) error {
	bqTable := bq.client.Dataset(bq.config.Dataset).Table(patchSchema.Name)
	metadata, err := bqTable.Metadata(bq.ctx)
	if err != nil {
		return fmt.Errorf("Error getting table %s metadata: %v", patchSchema.Name, err)
	}

	for columnName, column := range patchSchema.Columns {
		mappedColumnType, ok := SchemaToBigQuery[column.Type]
		if !ok {
			log.Println("Unknown BigQuery schema type:", column.Type.String())
			mappedColumnType = SchemaToBigQuery[schema.STRING]
		}
		metadata.Schema = append(metadata.Schema, &bigquery.FieldSchema{Name: columnName, Type: mappedColumnType})
	}

	updateReq := bigquery.TableMetadataToUpdate{Schema: metadata.Schema}
	if _, err := bqTable.Update(bq.ctx, updateReq, metadata.ETag); err != nil {
		var columns []string
		for _, column := range metadata.Schema {
			columns = append(columns, fmt.Sprintf("%s - %s", column.Name, column.Type))
		}
		return fmt.Errorf("Error patching %s BigQuery table with %s schema: %v", patchSchema.Name, strings.Join(columns, ","), err)
	}

	return nil
}

func (bq *BigQuery) Close() error {
	if err := bq.client.Close(); err != nil {
		return fmt.Errorf("Error closing BigQuery client: %v", err)
	}

	return nil
}

//Return true if google err is 404
func isNotFoundErr(err error) bool {
	e, ok := err.(*googleapi.Error)
	return ok && e.Code == http.StatusNotFound
}

func extractCredentials(config *GoogleConfig) option.ClientOption {
	if strings.Contains(config.KeyFile, "{") {
		return option.WithCredentialsJSON([]byte(config.KeyFile))
	} else {
		return option.WithCredentialsFile(config.KeyFile)
	}
}
