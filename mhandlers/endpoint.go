package mhandlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/dropbox/godropbox/container/set"
	"github.com/dropbox/godropbox/errors"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/pritunl/mongo-go-driver/bson"
	"github.com/pritunl/mongo-go-driver/bson/primitive"
	"github.com/pritunl/pritunl-zero/database"
	"github.com/pritunl/pritunl-zero/demo"
	"github.com/pritunl/pritunl-zero/endpoint"
	"github.com/pritunl/pritunl-zero/errortypes"
	"github.com/pritunl/pritunl-zero/event"
	"github.com/pritunl/pritunl-zero/utils"
	"github.com/sirupsen/logrus"
)

const (
	endpointWriteTimeout = 10 * time.Second
	endpointPingInterval = 30 * time.Second
	endpointPingWait     = 40 * time.Second
)

type endpointData struct {
	Id    primitive.ObjectID `json:"id"`
	Name  string             `json:"name"`
	Roles []string           `json:"roles"`
}

type endpointsData struct {
	Endpoints []*endpoint.Endpoint `json:"endpoints"`
	Count     int64                `json:"count"`
}

func endpointPut(c *gin.Context) {
	if demo.Blocked(c) {
		return
	}

	db := c.MustGet("db").(*database.Database)
	data := &endpointData{}

	endpointId, ok := utils.ParseObjectId(c.Param("endpoint_id"))
	if !ok {
		utils.AbortWithStatus(c, 400)
		return
	}

	err := c.Bind(data)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "handler: Bind error"),
		}
		utils.AbortWithError(c, 500, err)
		return
	}

	endpt, err := endpoint.Get(db, endpointId)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	endpt.Name = data.Name
	endpt.Roles = data.Roles

	fields := set.NewSet(
		"name",
		"roles",
	)

	errData, err := endpt.Validate(db)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	if errData != nil {
		c.JSON(400, errData)
		return
	}

	err = endpt.CommitFields(db, fields)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	event.PublishDispatch(db, "endpoint.change")

	c.JSON(200, endpt)
}

func endpointPost(c *gin.Context) {
	if demo.Blocked(c) {
		return
	}

	db := c.MustGet("db").(*database.Database)
	data := &endpointData{
		Name: "New Endpoint",
	}

	err := c.Bind(data)
	if err != nil {
		err = &errortypes.ParseError{
			errors.Wrap(err, "handler: Bind error"),
		}
		utils.AbortWithError(c, 500, err)
		return
	}

	endpt := &endpoint.Endpoint{
		Name:  data.Name,
		Roles: data.Roles,
	}

	errData, err := endpt.Validate(db)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	if errData != nil {
		c.JSON(400, errData)
		return
	}

	err = endpt.Insert(db)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	event.PublishDispatch(db, "endpoint.change")

	c.JSON(200, endpt)
}

func endpointDelete(c *gin.Context) {
	if demo.Blocked(c) {
		return
	}

	db := c.MustGet("db").(*database.Database)

	endpointId, ok := utils.ParseObjectId(c.Param("endpoint_id"))
	if !ok {
		utils.AbortWithStatus(c, 400)
		return
	}

	err := endpoint.Remove(db, endpointId)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	event.PublishDispatch(db, "endpoint.change")

	c.JSON(200, nil)
}

func endpointsDelete(c *gin.Context) {
	if demo.Blocked(c) {
		return
	}

	db := c.MustGet("db").(*database.Database)
	dta := []primitive.ObjectID{}

	err := c.Bind(&dta)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	err = endpoint.RemoveMulti(db, dta)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	event.PublishDispatch(db, "endpoint.change")

	c.JSON(200, nil)
}

func endpointsGet(c *gin.Context) {
	db := c.MustGet("db").(*database.Database)
	page, _ := strconv.ParseInt(c.Query("page"), 10, 0)
	pageCount, _ := strconv.ParseInt(c.Query("page_count"), 10, 0)

	query := bson.M{}

	endpointId, ok := utils.ParseObjectId(c.Query("id"))
	if ok {
		query["_id"] = endpointId
	}

	name := strings.TrimSpace(c.Query("name"))
	if name != "" {
		query["$or"] = []*bson.M{
			&bson.M{
				"name": &bson.M{
					"$regex":   fmt.Sprintf(".*%s.*", name),
					"$options": "i",
				},
			},
			&bson.M{
				"key": &bson.M{
					"$regex":   fmt.Sprintf(".*%s.*", name),
					"$options": "i",
				},
			},
		}
	}

	typ := strings.TrimSpace(c.Query("type"))
	if typ != "" {
		query["type"] = typ
	}

	organization, ok := utils.ParseObjectId(c.Query("organization"))
	if ok {
		query["organization"] = organization
	}

	endpoints, count, err := endpoint.GetAllPaged(
		db, &query, page, pageCount)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	dta := &endpointsData{
		Endpoints: endpoints,
		Count:     count,
	}

	c.JSON(200, dta)
}

func endpointCommGet(c *gin.Context) {
	db := c.MustGet("db").(*database.Database)
	socket := &endpoint.WebSocket{}

	endpointId, ok := utils.ParseObjectId(c.Param("endpoint_id"))
	if !ok {
		utils.AbortWithStatus(c, 400)
		return
	}

	endpt, err := endpoint.Get(db, endpointId)
	if err != nil {
		utils.AbortWithError(c, 500, err)
		return
	}

	defer func() {
		socket.Close()
		endpoint.WebSocketsLock.Lock()
		endpoint.WebSockets.Remove(socket)
		endpoint.WebSocketsLock.Unlock()
	}()

	endpoint.WebSocketsLock.Lock()
	endpoint.WebSockets.Add(socket)
	endpoint.WebSocketsLock.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	socket.Cancel = cancel

	conn, err := event.Upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "mhandlers: Failed to upgrade request"),
		}
		utils.AbortWithError(c, 500, err)
		return
	}
	socket.Conn = conn

	err = conn.SetReadDeadline(time.Now().Add(endpointPingWait))
	if err != nil {
		err = &errortypes.RequestError{
			errors.Wrap(err, "mhandlers: Failed to set read deadline"),
		}
		utils.AbortWithError(c, 500, err)
		return
	}

	conn.SetPongHandler(func(x string) (err error) {
		err = conn.SetReadDeadline(time.Now().Add(endpointPingWait))
		if err != nil {
			err = &errortypes.RequestError{
				errors.Wrap(err, "mhandlers: Failed to set read deadline"),
			}
			utils.AbortWithError(c, 500, err)
			return
		}

		return
	})

	ticker := time.NewTicker(endpointPingInterval)
	socket.Ticker = ticker

	go func() {
		defer func() {
			recover()
		}()
		for {
			msgType, msgByte, err := conn.ReadMessage()
			if err != nil {
				conn.Close()
				return
			}

			if msgType != websocket.TextMessage {
				continue
			}

			msg := string(msgByte)

			sepIndex := strings.Index(msg, ":")
			if sepIndex == -1 {
				continue
			}

			docType := msg[:sepIndex]
			doc := msg[sepIndex+1:]

			err = endpoint.ProcessDoc(db, endpt, docType, doc)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error": err,
				}).Error("mhandlers: Failed to process doc")

				conn.Close()
				return
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			err = conn.WriteControl(websocket.PingMessage, []byte{},
				time.Now().Add(endpointWriteTimeout))
			if err != nil {
				err = &errortypes.RequestError{
					errors.Wrap(err,
						"mhandlers: Failed to set write control"),
				}
				return
			}
		}
	}
}
