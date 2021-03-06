// Copyright 2017 Northern.tech AS
//
//    Licensed under the Apache License, Version 2.0 (the "License");
//    you may not use this file except in compliance with the License.
//    You may obtain a copy of the License at
//
//        http://www.apache.org/licenses/LICENSE-2.0
//
//    Unless required by applicable law or agreed to in writing, software
//    distributed under the License is distributed on an "AS IS" BASIS,
//    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//    See the License for the specific language governing permissions and
//    limitations under the License.
package http

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ant0ine/go-json-rest/rest"
	"github.com/asaskevich/govalidator"
	"github.com/mendersoftware/go-lib-micro/log"
	u "github.com/mendersoftware/go-lib-micro/rest_utils"
	"github.com/pkg/errors"

	inventory "github.com/mendersoftware/inventory/inv"
	"github.com/mendersoftware/inventory/model"
	"github.com/mendersoftware/inventory/store"
	"github.com/mendersoftware/inventory/utils"
	"github.com/mendersoftware/inventory/utils/identity"
	"regexp"
)

const (
	uriDevices       = "/api/0.1.0/devices"
	uriDevice        = "/api/0.1.0/devices/:id"
	uriDeviceGroups  = "/api/0.1.0/devices/:id/group"
	uriDeviceGroup   = "/api/0.1.0/devices/:id/group/:name"
	uriAttributes    = "/api/0.1.0/attributes"
	uriGroups        = "/api/0.1.0/groups"
	uriGroupsDevices = "/api/0.1.0/groups/:name/devices"
)

const (
	queryParamSort           = "sort"
	queryParamHasGroup       = "has_group"
	queryParamValueSeparator = ":"
	sortOrderAsc             = "asc"
	sortOrderDesc            = "desc"
	sortAttributeNameIdx     = 0
	sortOrderIdx             = 1
	filterEqOperatorIdx      = 0
)

// model of device's group name response at /devices/:id/group endpoint
type InventoryApiGroup struct {
	Group string `json:"group" valid:"required"`
}

type InventoryHandlers struct {
	inventory inventory.InventoryApp
}

// return an ApiHandler for device admission app
func NewInventoryApiHandlers(i inventory.InventoryApp) ApiHandler {
	return &InventoryHandlers{
		inventory: i,
	}
}

func (i *InventoryHandlers) GetApp() (rest.App, error) {
	routes := []*rest.Route{
		rest.Get(uriDevices, i.GetDevicesHandler),
		rest.Post(uriDevices, i.AddDeviceHandler),
		rest.Get(uriDevice, i.GetDeviceHandler),
		rest.Delete(uriDevice, i.DeleteDeviceHandler),
		rest.Delete(uriDeviceGroup, i.DeleteDeviceGroupHandler),
		rest.Patch(uriAttributes, i.PatchDeviceAttributesHandler),
		rest.Put(uriDeviceGroups, i.AddDeviceToGroupHandler),
		rest.Get(uriDeviceGroups, i.GetDeviceGroupHandler),
		rest.Get(uriGroups, i.GetGroupsHandler),
		rest.Get(uriGroupsDevices, i.GetDevicesByGroup),
	}

	routes = append(routes)

	app, err := rest.MakeRouter(
		// augment routes with OPTIONS handler
		AutogenOptionsRoutes(routes, AllowHeaderOptionsGenerator)...,
	)
	if err != nil {
		return nil, errors.Wrap(err, "failed to create router")
	}

	return app, nil

}

// `sort` paramater value is an attribute name with optional direction (desc or asc)
// separated by colon (:)
//
// eg. `sort=attr_name1` or `sort=attr_name1:asd`
func parseSortParam(r *rest.Request) (*store.Sort, error) {
	sortStr, err := utils.ParseQueryParmStr(r, queryParamSort, false, nil)
	if err != nil {
		return nil, err
	}
	if sortStr == "" {
		return nil, nil
	}
	sortValArray := strings.Split(sortStr, queryParamValueSeparator)
	sort := store.Sort{AttrName: sortValArray[sortAttributeNameIdx]}
	if len(sortValArray) == 2 {
		sortOrder := sortValArray[sortOrderIdx]
		if sortOrder != sortOrderAsc && sortOrder != sortOrderDesc {
			return nil, errors.New("invalid sort order")
		}
		sort.Ascending = sortOrder == sortOrderAsc
	}
	return &sort, nil
}

// Filter paramaters name are attributes name. Value can be prefixed
// with equality operator code (`eq` for =), separated from value by colon (:).
// Equality operator default value is `eq`
//
// eg. `attr_name1=value1` or `attr_name1=eq:value1`
func parseFilterParams(r *rest.Request) ([]store.Filter, error) {
	knownParams := []string{utils.PageName, utils.PerPageName, queryParamSort, queryParamHasGroup}
	filters := make([]store.Filter, 0)
	var filter store.Filter
	for name := range r.URL.Query() {
		if utils.ContainsString(name, knownParams) {
			continue
		}
		valueStr, err := utils.ParseQueryParmStr(r, name, false, nil)
		if err != nil {
			return nil, err
		}
		valueStrArray := strings.Split(valueStr, queryParamValueSeparator)
		filter = store.Filter{AttrName: name}
		valueIdx := 0
		if len(valueStrArray) == 2 {
			valueIdx = 1
			switch valueStrArray[filterEqOperatorIdx] {
			case "eq":
				filter.Operator = store.Eq
			default:
				return nil, errors.New("invalid filter operator")
			}
		} else {
			filter.Operator = store.Eq
		}
		filter.Value = valueStrArray[valueIdx]
		floatValue, err := strconv.ParseFloat(filter.Value, 64)
		if err == nil {
			filter.ValueFloat = &floatValue
		}

		filters = append(filters, filter)
	}
	return filters, nil
}

func (i *InventoryHandlers) GetDevicesHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	page, perPage, err := utils.ParsePagination(r)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	hasGroup, err := utils.ParseQueryParmBool(r, queryParamHasGroup, false, nil)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	sort, err := parseSortParam(r)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	filters, err := parseFilterParams(r)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	//get one extra device to see if there's a 'next' page
	devs, err := i.inventory.ListDevices(ctx, int((page-1)*perPage), int(perPage+1), filters, sort, hasGroup)
	if err != nil {
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}

	len := len(devs)
	hasNext := false
	if uint64(len) > perPage {
		hasNext = true
		len = int(perPage)
	}

	links := utils.MakePageLinkHdrs(r, page, perPage, hasNext)

	for _, l := range links {
		w.Header().Add("Link", l)
	}
	w.WriteJson(devs[:len])
}

func (i *InventoryHandlers) GetDeviceHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	deviceID := r.PathParam("id")

	dev, err := i.inventory.GetDevice(ctx, model.DeviceID(deviceID))
	if err != nil {
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}
	if dev == nil {
		u.RestErrWithLog(w, r, l, store.ErrDevNotFound, http.StatusNotFound)
		return
	}

	w.WriteJson(dev)
}

func (i *InventoryHandlers) DeleteDeviceHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	deviceID := r.PathParam("id")

	err := i.inventory.DeleteDevice(ctx, model.DeviceID(deviceID))
	if err != nil && err != store.ErrDevNotFound {
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (i *InventoryHandlers) AddDeviceHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	dev, err := parseDevice(r)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	err = i.inventory.AddDevice(ctx, dev)
	if err != nil {
		if cause := errors.Cause(err); cause != nil && cause == store.ErrDuplicatedDeviceId {
			u.RestErrWithLogMsg(w, r, l, err, http.StatusConflict, "device with specified ID already exists")
			return
		}
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}

	w.Header().Add("Location", "devices/"+dev.ID.String())
	w.WriteHeader(http.StatusCreated)
}

func (i *InventoryHandlers) PatchDeviceAttributesHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	//get device ID from JWT token
	idata, err := identity.ExtractIdentityFromHeaders(r.Header)
	if err != nil {
		u.RestErrWithLogMsg(w, r, l, err, http.StatusUnauthorized, "unauthorized")
		return
	}

	//extract attributes from body
	attrs, err := parseAttributes(r)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	//upsert the attributes
	err = i.inventory.UpsertAttributes(ctx, model.DeviceID(idata.Subject), attrs)
	if err != nil {
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (i *InventoryHandlers) DeleteDeviceGroupHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	deviceID := r.PathParam("id")
	groupName := r.PathParam("name")

	err := i.inventory.UnsetDeviceGroup(ctx, model.DeviceID(deviceID), model.GroupName(groupName))
	if err != nil {
		cause := errors.Cause(err)
		if cause != nil {
			if cause.Error() == store.ErrDevNotFound.Error() {
				u.RestErrWithLog(w, r, l, err, http.StatusNotFound)
				return
			}
		}
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (i *InventoryHandlers) AddDeviceToGroupHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	devId := r.PathParam("id")

	var group InventoryApiGroup
	err := r.DecodeJsonPayload(&group)
	if err != nil {
		u.RestErrWithLog(
			w, r, l, errors.Wrap(err, "failed to decode device group data"),
			http.StatusBadRequest)
		return
	}
	if _, err = govalidator.ValidateStruct(group); err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	if !regexp.MustCompile("^[A-Za-z0-9_-]*$").MatchString(group.Group) {
		u.RestErrWithLog(w, r, l, errors.New("Group name can only contain: upper/lowercase alphanum, -(dash), _(underscore)"), http.StatusBadRequest)
		return
	}

	err = i.inventory.UpdateDeviceGroup(ctx, model.DeviceID(devId), model.GroupName(group.Group))
	if err != nil {
		if cause := errors.Cause(err); cause != nil && cause == store.ErrDevNotFound {
			u.RestErrWithLog(w, r, l, err, http.StatusNotFound)
			return
		}
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (i *InventoryHandlers) GetDevicesByGroup(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	group := r.PathParam("name")

	page, perPage, err := utils.ParsePagination(r)
	if err != nil {
		u.RestErrWithLog(w, r, l, err, http.StatusBadRequest)
		return
	}

	//get one extra device to see if there's a 'next' page
	ids, err := i.inventory.ListDevicesByGroup(ctx, model.GroupName(group), int((page-1)*perPage), int(perPage+1))
	if err != nil {
		if err == store.ErrGroupNotFound {
			u.RestErrWithLog(w, r, l, err, http.StatusNotFound)
		} else {
			u.RestErrWithLogInternal(w, r, l, err)
		}
		return
	}

	len := len(ids)
	hasNext := false
	if uint64(len) > perPage {
		hasNext = true
		len = int(perPage)
	}

	links := utils.MakePageLinkHdrs(r, page, perPage, hasNext)
	for _, l := range links {
		w.Header().Add("Link", l)
	}
	w.WriteJson(ids[:len])

}

func parseDevice(r *rest.Request) (*model.Device, error) {
	dev := model.Device{}

	//decode body
	err := r.DecodeJsonPayload(&dev)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode request body")
	}

	if err := dev.Validate(); err != nil {
		return nil, err
	}

	return &dev, nil
}

func parseAttributes(r *rest.Request) (model.DeviceAttributes, error) {
	var attrs model.DeviceAttributes

	err := r.DecodeJsonPayload(&attrs)
	if err != nil {
		return nil, errors.Wrap(err, "failed to decode request body")
	}

	for _, a := range attrs {
		if _, err = govalidator.ValidateStruct(a); err != nil {
			return nil, err
		}
	}

	return attrs, nil
}

func (i *InventoryHandlers) GetGroupsHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	groups, err := i.inventory.ListGroups(ctx)
	if err != nil {
		u.RestErrWithLogInternal(w, r, l, err)
		return
	}

	if groups == nil {
		groups = []model.GroupName{}
	}

	w.WriteJson(groups)
}

func (i *InventoryHandlers) GetDeviceGroupHandler(w rest.ResponseWriter, r *rest.Request) {
	ctx := r.Context()

	l := log.FromContext(ctx)

	deviceID := r.PathParam("id")

	group, err := i.inventory.GetDeviceGroup(ctx, model.DeviceID(deviceID))
	if err != nil {
		if err == store.ErrDevNotFound {
			u.RestErrWithLog(w, r, l, store.ErrDevNotFound, http.StatusNotFound)
		} else {
			u.RestErrWithLogInternal(w, r, l, err)
		}
		return
	}

	ret := map[string]*model.GroupName{"group": nil}

	if group != "" {
		ret["group"] = &group
	}

	w.WriteJson(ret)
}
