package main

import (
	"errors"
	"fmt"
	"log"
	"testing"

	gbcsv "github.com/zoobst/gobi/gbCsv"
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
	Geometry  Geometry `csv:"geometry" dtype:"geometry"`
	Placename string   `csv:"placename"`
}

var (
	testDir              = "testData"
	testFileName         = "titanic_test"
	testGeometryFileName = "israel"
	ErrFailedTest        = errors.New("failed test: %s")
	df                   *DataFrame
)

func TestReadCSVFromType(t *testing.T) {
	idf, err := ReadCSVFromType(TestCSVTypes{}, fmt.Sprintf("%s/%s.csv", testDir, testFileName), gbcsv.CsvReadOptions{})
	if err != nil {
		t.Errorf("reading csv from type failed: %v", err)
	}

	df = idf

	if rows, cols := idf.Shape(); rows > 0 && cols > 0 {
		t.Errorf("reading csv from type failed")
	}
}

/* func TestReadGeometryCSVFromType() bool {
	idf, err := ReadCSVFromType(TestGeometryCSVTypes{}, fmt.Sprintf("%s/%s.csv", testDir, testGeometryFileName), gbcsv.CsvReadOptions{})
	if err != nil {
		log.Fatal(err)
	}

	df = idf

	if rows, cols := df.Shape(); rows > 0 && cols > 0 {
		return true
	}

	return false
} */

func TestReadGeometryCSVFromTypeUsingArrow(t *testing.T) {
	idf, err := ReadCSVFromTypeUsingArrow(
		TestGeometryCSVTypes{},
		fmt.Sprintf("%s/%s.csv",
			testDir,
			testGeometryFileName,
		))
	if err != nil {
		t.Errorf("reading csv from type failed: %v", err)
	}

	df = idf

	if rows, cols := df.Shape(); rows > 0 && cols > 0 {
		t.Errorf("reading csv from type failed")
	}
}

func TestIloc(t *testing.T) {
	if df2, err := df.Iloc(1); err == nil {
		if rows, cols := df2.Shape(); rows > 0 && cols > 0 {
			log.Println(df2)
			t.Errorf("test ILoc failed: %v", err)
		} else {
			t.Errorf("test ILoc failed: %v", err)
		}
	} else {
		t.Errorf("test ILoc failed: %v", err)
	}
}

func TestCol(t *testing.T) {
	if v, err := df.Col("geometry"); err == nil {
		if df2, err := v.Head(5); err == nil {
			log.Println(df2)
		} else {
			t.Errorf("test Col failed: %v", err)
		}
		if df2, err := v.Tail(5); err == nil {
			log.Println(df2)
		} else {
			t.Errorf("test Col failed: %v", err)
		}
	}
	t.Errorf("test Col failed")
}

func TestArea(t *testing.T) {
	if v, err := df.Col("geometry"); err == nil {
		area, err := v.Area(0, "km")
		if err != nil {
			log.Println(err)
			t.Errorf("test Area failed: %v", err)
		}
		log.Println(area)
	}
}
