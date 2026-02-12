package models

type MenuItem struct {
	ID       string
	Category string // "food", "drink", "dessert"
	Name     string
	Price    int64
}

const (
	CategoryFood    = "food"
	CategoryDrink   = "drink"
	CategoryDessert = "dessert"
)
