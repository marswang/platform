// Copyright (c) 2015 Spinpunch, Inc. All Rights Reserved.
// See License.txt for license information.

package store

import (
	l4g "code.google.com/p/log4go"
	"crypto/aes"
	"crypto/cipher"
	crand "crypto/rand"
	dbsql "database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/go-gorp/gorp"
	_ "github.com/go-sql-driver/mysql"
	"github.com/mattermost/platform/model"
	"github.com/mattermost/platform/utils"
	"io"
	sqltrace "log"
	"math/rand"
	"os"
	"time"
)

type SqlStore struct {
	master   *gorp.DbMap
	replicas []*gorp.DbMap
	team     TeamStore
	channel  ChannelStore
	post     PostStore
	user     UserStore
	audit    AuditStore
	session  SessionStore
}

func NewSqlStore() Store {

	sqlStore := &SqlStore{}

	sqlStore.master = setupConnection("master", utils.Cfg.SqlSettings.DriverName,
		utils.Cfg.SqlSettings.DataSource, utils.Cfg.SqlSettings.MaxIdleConns,
		utils.Cfg.SqlSettings.MaxOpenConns, utils.Cfg.SqlSettings.Trace)

	sqlStore.replicas = make([]*gorp.DbMap, len(utils.Cfg.SqlSettings.DataSourceReplicas))
	for i, replica := range utils.Cfg.SqlSettings.DataSourceReplicas {
		sqlStore.replicas[i] = setupConnection(fmt.Sprintf("replica-%v", i), utils.Cfg.SqlSettings.DriverName, replica,
			utils.Cfg.SqlSettings.MaxIdleConns, utils.Cfg.SqlSettings.MaxOpenConns,
			utils.Cfg.SqlSettings.Trace)
	}

	sqlStore.team = NewSqlTeamStore(sqlStore)
	sqlStore.channel = NewSqlChannelStore(sqlStore)
	sqlStore.post = NewSqlPostStore(sqlStore)
	sqlStore.user = NewSqlUserStore(sqlStore)
	sqlStore.audit = NewSqlAuditStore(sqlStore)
	sqlStore.session = NewSqlSessionStore(sqlStore)

	sqlStore.master.CreateTablesIfNotExists()

	sqlStore.team.(*SqlTeamStore).CreateIndexesIfNotExists()
	sqlStore.channel.(*SqlChannelStore).CreateIndexesIfNotExists()
	sqlStore.post.(*SqlPostStore).CreateIndexesIfNotExists()
	sqlStore.user.(*SqlUserStore).CreateIndexesIfNotExists()
	sqlStore.audit.(*SqlAuditStore).CreateIndexesIfNotExists()
	sqlStore.session.(*SqlSessionStore).CreateIndexesIfNotExists()

	sqlStore.team.(*SqlTeamStore).UpgradeSchemaIfNeeded()
	sqlStore.channel.(*SqlChannelStore).UpgradeSchemaIfNeeded()
	sqlStore.post.(*SqlPostStore).UpgradeSchemaIfNeeded()
	sqlStore.user.(*SqlUserStore).UpgradeSchemaIfNeeded()
	sqlStore.audit.(*SqlAuditStore).UpgradeSchemaIfNeeded()
	sqlStore.session.(*SqlSessionStore).UpgradeSchemaIfNeeded()

	return sqlStore
}

func setupConnection(con_type string, driver string, dataSource string, maxIdle int, maxOpen int, trace bool) *gorp.DbMap {

	db, err := dbsql.Open(driver, dataSource)
	if err != nil {
		l4g.Critical("Failed to open sql connection to '%v' err:%v", dataSource, err)
		time.Sleep(time.Second)
		panic("Failed to open sql connection" + err.Error())
	}

	l4g.Info("Pinging sql %v database at '%v'", con_type, dataSource)
	err = db.Ping()
	if err != nil {
		l4g.Critical("Failed to ping db err:%v", err)
		time.Sleep(time.Second)
		panic("Failed to open sql connection " + err.Error())
	}

	db.SetMaxIdleConns(maxIdle)
	db.SetMaxOpenConns(maxOpen)

	var dbmap *gorp.DbMap

	if driver == "sqlite3" {
		dbmap = &gorp.DbMap{Db: db, TypeConverter: mattermConverter{}, Dialect: gorp.SqliteDialect{}}
	} else if driver == "mysql" {
		dbmap = &gorp.DbMap{Db: db, TypeConverter: mattermConverter{}, Dialect: gorp.MySQLDialect{Engine: "InnoDB", Encoding: "UTF8"}}
	} else {
		l4g.Critical("Failed to create dialect specific driver")
		time.Sleep(time.Second)
		panic("Failed to create dialect specific driver " + err.Error())
	}

	if trace {
		dbmap.TraceOn("", sqltrace.New(os.Stdout, "sql-trace:", sqltrace.Lmicroseconds))
	}

	return dbmap
}

func (ss SqlStore) CreateColumnIfNotExists(tableName string, columnName string, afterName string, colType string, defaultValue string) bool {
	count, err := ss.GetMaster().SelectInt(
		`SELECT 
		    COUNT(0) AS column_exists
		FROM
		    information_schema.COLUMNS
		WHERE
		    TABLE_SCHEMA = DATABASE()
		        AND TABLE_NAME = ?
		        AND COLUMN_NAME = ?`,
		tableName,
		columnName,
	)
	if err != nil {
		l4g.Critical("Failed to check if column exists %v", err)
		time.Sleep(time.Second)
		panic("Failed to check if column exists " + err.Error())
	}

	if count > 0 {
		return false
	}

	_, err = ss.GetMaster().Exec("ALTER TABLE " + tableName + " ADD " + columnName + " " + colType + " DEFAULT '" + defaultValue + "'" + " AFTER " + afterName)
	if err != nil {
		l4g.Critical("Failed to create column %v", err)
		time.Sleep(time.Second)
		panic("Failed to create column " + err.Error())
	}

	return true
}

func (ss SqlStore) RemoveColumnIfExists(tableName string, columnName string) bool {
	count, err := ss.GetMaster().SelectInt(
		`SELECT 
		    COUNT(0) AS column_exists
		FROM
		    information_schema.COLUMNS
		WHERE
		    TABLE_SCHEMA = DATABASE()
		        AND TABLE_NAME = ?
		        AND COLUMN_NAME = ?`,
		tableName,
		columnName,
	)
	if err != nil {
		l4g.Critical("Failed to check if column exists %v", err)
		time.Sleep(time.Second)
		panic("Failed to check if column exists " + err.Error())
	}

	if count == 0 {
		return false
	}

	_, err = ss.GetMaster().Exec("ALTER TABLE " + tableName + " DROP COLUMN " + columnName)
	if err != nil {
		l4g.Critical("Failed to drop column %v", err)
		time.Sleep(time.Second)
		panic("Failed to drop column " + err.Error())
	}

	return true
}

func (ss SqlStore) CreateIndexIfNotExists(indexName string, tableName string, columnName string) {
	ss.createIndexIfNotExists(indexName, tableName, columnName, false)
}

func (ss SqlStore) CreateFullTextIndexIfNotExists(indexName string, tableName string, columnName string) {
	ss.createIndexIfNotExists(indexName, tableName, columnName, true)
}

func (ss SqlStore) createIndexIfNotExists(indexName string, tableName string, columnName string, fullText bool) {
	count, err := ss.GetMaster().SelectInt("SELECT COUNT(0) AS index_exists FROM information_schema.statistics WHERE TABLE_SCHEMA = DATABASE() and table_name = ? AND index_name = ?", tableName, indexName)
	if err != nil {
		l4g.Critical("Failed to check index", err)
		time.Sleep(time.Second)
		panic("Failed to check index" + err.Error())
	}

	if count > 0 {
		return
	}

	fullTextIndex := ""
	if fullText {
		fullTextIndex = " FULLTEXT "
	}

	_, err = ss.GetMaster().Exec("CREATE " + fullTextIndex + " INDEX " + indexName + " ON " + tableName + " (" + columnName + ")")
	if err != nil {
		l4g.Critical("Failed to create index", err)
		time.Sleep(time.Second)
		panic("Failed to create index " + err.Error())
	}
}

func (ss SqlStore) GetMaster() *gorp.DbMap {
	return ss.master
}

func (ss SqlStore) GetReplica() *gorp.DbMap {
	return ss.replicas[rand.Intn(len(ss.replicas))]
}

func (ss SqlStore) GetAllConns() []*gorp.DbMap {
	all := make([]*gorp.DbMap, len(ss.replicas)+1)
	copy(all, ss.replicas)
	all[len(ss.replicas)] = ss.master
	return all
}

func (ss SqlStore) Close() {
	l4g.Info("Closing SqlStore")
	ss.master.Db.Close()
	for _, replica := range ss.replicas {
		replica.Db.Close()
	}
}

func (ss SqlStore) Team() TeamStore {
	return ss.team
}

func (ss SqlStore) Channel() ChannelStore {
	return ss.channel
}

func (ss SqlStore) Post() PostStore {
	return ss.post
}

func (ss SqlStore) User() UserStore {
	return ss.user
}

func (ss SqlStore) Session() SessionStore {
	return ss.session
}

func (ss SqlStore) Audit() AuditStore {
	return ss.audit
}

type mattermConverter struct{}

func (me mattermConverter) ToDb(val interface{}) (interface{}, error) {

	switch t := val.(type) {
	case model.StringMap:
		return model.MapToJson(t), nil
	case model.StringArray:
		return model.ArrayToJson(t), nil
	case model.EncryptStringMap:
		return encrypt([]byte(utils.Cfg.SqlSettings.AtRestEncryptKey), model.MapToJson(t))
	}

	return val, nil
}

func (me mattermConverter) FromDb(target interface{}) (gorp.CustomScanner, bool) {
	switch target.(type) {
	case *model.StringMap:
		binder := func(holder, target interface{}) error {
			s, ok := holder.(*string)
			if !ok {
				return errors.New("FromDb: Unable to convert StringMap to *string")
			}
			b := []byte(*s)
			return json.Unmarshal(b, target)
		}
		return gorp.CustomScanner{new(string), target, binder}, true
	case *model.StringArray:
		binder := func(holder, target interface{}) error {
			s, ok := holder.(*string)
			if !ok {
				return errors.New("FromDb: Unable to convert StringArray to *string")
			}
			b := []byte(*s)
			return json.Unmarshal(b, target)
		}
		return gorp.CustomScanner{new(string), target, binder}, true
	case *model.EncryptStringMap:
		binder := func(holder, target interface{}) error {
			s, ok := holder.(*string)
			if !ok {
				return errors.New("FromDb: Unable to convert EncryptStringMap to *string")
			}

			ue, err := decrypt([]byte(utils.Cfg.SqlSettings.AtRestEncryptKey), *s)
			if err != nil {
				return err
			}

			b := []byte(ue)
			return json.Unmarshal(b, target)
		}
		return gorp.CustomScanner{new(string), target, binder}, true
	}

	return gorp.CustomScanner{}, false
}

func encrypt(key []byte, text string) (string, error) {

	if text == "" || text == "{}" {
		return "", nil
	}

	plaintext := []byte(text)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	ciphertext := make([]byte, aes.BlockSize+len(plaintext))
	iv := ciphertext[:aes.BlockSize]
	if _, err := io.ReadFull(crand.Reader, iv); err != nil {
		return "", err
	}

	stream := cipher.NewCFBEncrypter(block, iv)
	stream.XORKeyStream(ciphertext[aes.BlockSize:], plaintext)

	return base64.URLEncoding.EncodeToString(ciphertext), nil
}

func decrypt(key []byte, cryptoText string) (string, error) {

	if cryptoText == "" || cryptoText == "{}" {
		return "{}", nil
	}

	ciphertext, _ := base64.URLEncoding.DecodeString(cryptoText)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	if len(ciphertext) < aes.BlockSize {
		return "", errors.New("ciphertext too short")
	}
	iv := ciphertext[:aes.BlockSize]
	ciphertext = ciphertext[aes.BlockSize:]

	stream := cipher.NewCFBDecrypter(block, iv)

	stream.XORKeyStream(ciphertext, ciphertext)

	return fmt.Sprintf("%s", ciphertext), nil
}
