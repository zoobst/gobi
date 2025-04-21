package tests

type TestCSVTypes struct {
	PassengerID int     `csv:"PassengerId"`
	Survived    bool    `csv:"Survived"`
	Pclass      int     `csv:"Pclass"`
	Name        string  `csv:"Name"`
	Sex         string  `csv:"Sex"`
	Age         float64 `csv:"Age"`
	SibSp       int     `csv:"SibSp"`
	Parch       string  `csv:"Parch"`
	Ticket      string  `csv:"Ticket"`
	Fare        float64 `csv:"Fare"`
	Cabin       string  `csv:"Cabin"`
	Embarked    string  `csv:"Embarked"`
}
