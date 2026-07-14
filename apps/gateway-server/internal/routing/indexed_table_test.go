package routing

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLinearTable_ExactMatch(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.list",
		Method:       "GET",
		PathTemplate: "/users",
	})

	route, params, ok := table.Lookup("GET", "/users")
	assert.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.users.list", route.Subject)
	assert.Empty(t, params)
}

func TestLinearTable_PathParamMatch(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.get",
		Method:       "GET",
		PathTemplate: "/users/:id",
	})

	route, params, ok := table.Lookup("GET", "/users/42")
	assert.True(t, ok)
	assert.Equal(t, "users-svc__microservice.cmd.users.get", route.Subject)
	assert.Equal(t, map[string]string{"id": "42"}, params)
}

func TestLinearTable_MultipleParams(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "orders-svc__microservice.cmd.orders.item.get",
		Method:       "GET",
		PathTemplate: "/orders/:orderId/items/:itemId",
	})

	route, params, ok := table.Lookup("GET", "/orders/abc/items/xyz")
	assert.True(t, ok)
	assert.Equal(t, "orders-svc__microservice.cmd.orders.item.get", route.Subject)
	assert.Equal(t, map[string]string{"orderId": "abc", "itemId": "xyz"}, params)
}

func TestLinearTable_MissReturnsFalse(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.list",
		Method:       "GET",
		PathTemplate: "/users",
	})

	route, params, ok := table.Lookup("GET", "/unknown")
	assert.False(t, ok)
	assert.Equal(t, Route{}, route)
	assert.Nil(t, params)
}

func TestLinearTable_MethodMismatch(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.list",
		Method:       "GET",
		PathTemplate: "/users",
	})

	_, _, ok := table.Lookup("POST", "/users")
	assert.False(t, ok)
}

func TestLinearTable_DifferentPathSegmentCountNoMatch(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.get",
		Method:       "GET",
		PathTemplate: "/users/:id",
	})

	// "/users/42/extra" has three segments; the template has two.
	_, _, ok := table.Lookup("GET", "/users/42/extra")
	assert.False(t, ok)
}

func TestLinearTable_Methods(t *testing.T) {
	table := newIndexedTable()
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.list",
		Method:       "GET",
		PathTemplate: "/users",
	})
	table.add(Route{
		Subject:      "users-svc__microservice.cmd.users.create",
		Method:       "POST",
		PathTemplate: "/users",
	})

	methods := table.Methods("/users")
	assert.ElementsMatch(t, []string{"GET", "POST"}, methods)
}
