package main

import (
	"errors"
	"fmt"
	"log"

	gbcsv "github.com/zoobst/gobi/gbCsv"
	gTypes "github.com/zoobst/gobi/globalTypes"
)

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

type TestGeometryCSVTypes struct {
	Geometry  gTypes.Geometry `csv:"geometry" dtype:"geometry"`
	Placename string          `csv:"placename"`
}

var (
	testDir              = "testData"
	testFileName         = "titanic_test"
	testGeometryFileName = "israel"
	ErrFailedTest        = errors.New("failed test: %s")
	df                   *DataFrame
)

func TestReadCSVFromType() bool {
	idf, err := ReadCSVFromType(TestCSVTypes{}, fmt.Sprintf("%s/%s.csv", testDir, testFileName), gbcsv.CsvReadOptions{})
	if err != nil {
		log.Fatal(err)
	}

	df = idf

	if rows, cols := idf.Shape(); rows > 0 && cols > 0 {
		return true
	}

	return false
}

func TestReadGeometryCSVFromType() bool {
	idf, err := ReadCSVFromType(TestGeometryCSVTypes{}, fmt.Sprintf("%s/%s.csv", testDir, testGeometryFileName), gbcsv.CsvReadOptions{})
	if err != nil {
		log.Fatal(err)
	}

	df = idf

	if rows, cols := df.Shape(); rows > 0 && cols > 0 {
		return true
	}

	return false
}

func TestIloc() bool {
	if df2, err := df.Iloc(0); err == nil {
		if rows, cols := df2.Shape(); rows > 0 && cols > 0 {
			log.Println(df2)
			return true
		}
		return false
	} else {
		return false
	}
}

func TestCol() bool {
	if v, err := df.Col("Sex"); err == nil {
		if df2, err := v.Head(5); err == nil {
			log.Println(df2)
		} else {
			return false
		}
		if df2, err := v.Tail(5); err == nil {
			log.Println(df2)
			return true
		} else {
			return false
		}
	}
	return false
}
