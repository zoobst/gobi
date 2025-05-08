package geometry

var (
	WGS84 CRS = CRS{
		Name:      "WGS 84",
		AreaOfUse: "World",
		BoundBox:  [4]float64{-180.0, -90.0, 180.0, 90.0},
		EPSG:      4326,
		Projected: false,
	}
	PSEUDOMERCATOR CRS = CRS{
		Name:      "WGS 84 / PSEUDO-MERCATOR",
		AreaOfUse: "World between 85.06°S and 85.06°N",
		BoundBox:  [4]float64{-20026376.39, -20048966.1, 20026376.39, 20048966.1},
		EPSG:      3857,
		Projected: true,
	}
	UTMZONE1N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 1N",
		AreaOfUse: "Between 0°E and 6°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{0.0, 0.0, 6.0, 84.0},
		EPSG:      32601,
		Projected: true,
		Zone:      "1N",
	}
	UTMZONE1S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 1S",
		AreaOfUse: "Between 0°E and 6°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{0.0, -80.0, 6.0, 0.0},
		EPSG:      32701,
		Projected: true,
		Zone:      "1S",
	}
	UTMZONE2N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 2N",
		AreaOfUse: "Between 6°E and 12°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{6.0, 0.0, 12.0, 84.0},
		EPSG:      32602,
		Projected: true,
		Zone:      "2N",
	}
	UTMZONE2S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 2S",
		AreaOfUse: "Between 6°E and 12°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{6.0, -80.0, 12.0, 0.0},
		EPSG:      32702,
		Projected: true,
		Zone:      "2S",
	}
	UTMZONE3N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 3N",
		AreaOfUse: "Between 12°E and 18°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{12.0, 0.0, 18.0, 84.0},
		EPSG:      32603,
		Projected: true,
		Zone:      "3N",
	}
	UTMZONE3S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 3S",
		AreaOfUse: "Between 12°E and 18°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{12.0, -80.0, 18.0, 0.0},
		EPSG:      32703,
		Projected: true,
		Zone:      "3S",
	}
	UTMZONE4N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 4N",
		AreaOfUse: "Between 18°E and 24°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{18.0, 0.0, 24.0, 84.0},
		EPSG:      32604,
		Projected: true,
		Zone:      "4N",
	}
	UTMZONE4S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 4S",
		AreaOfUse: "Between 18°E and 24°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{18.0, -80.0, 24.0, 0.0},
		EPSG:      32704,
		Projected: true,
		Zone:      "4S",
	}
	UTMZONE5N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 5N",
		AreaOfUse: "Between 24°E and 30°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{24.0, 0.0, 30.0, 84.0},
		EPSG:      32605,
		Projected: true,
		Zone:      "5N",
	}
	UTMZONE5S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 5S",
		AreaOfUse: "Between 24°E and 30°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{24.0, -80.0, 30.0, 0.0},
		EPSG:      32705,
		Projected: true,
		Zone:      "5S",
	}
	UTMZONE6N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 6N",
		AreaOfUse: "Between 30°E and 36°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{30.0, 0.0, 36.0, 84.0},
		EPSG:      32606,
		Projected: true,
		Zone:      "6N",
	}
	UTMZONE6S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 6S",
		AreaOfUse: "Between 30°E and 36°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{30.0, -80.0, 36.0, 0.0},
		EPSG:      32706,
		Projected: true,
		Zone:      "6S",
	}
	UTMZONE7N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 7N",
		AreaOfUse: "Between 36°E and 42°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{36.0, 0.0, 42.0, 84.0},
		EPSG:      32607,
		Projected: true,
		Zone:      "7N",
	}
	UTMZONE7S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 7S",
		AreaOfUse: "Between 36°E and 42°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{36.0, -80.0, 42.0, 0.0},
		EPSG:      32707,
		Projected: true,
		Zone:      "7S",
	}
	UTMZONE8N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 8N",
		AreaOfUse: "Between 42°E and 48°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{42.0, 0.0, 48.0, 84.0},
		EPSG:      32608,
		Projected: true,
		Zone:      "8N",
	}
	UTMZONE8S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 8S",
		AreaOfUse: "Between 42°E and 48°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{42.0, -80.0, 48.0, 0.0},
		EPSG:      32708,
		Projected: true,
		Zone:      "8S",
	}
	UTMZONE9N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 9N",
		AreaOfUse: "Between 48°E and 54°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{48.0, 0.0, 54.0, 84.0},
		EPSG:      32609,
		Projected: true,
		Zone:      "9N",
	}
	UTMZONE9S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 9S",
		AreaOfUse: "Between 48°E and 54°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{48.0, -80.0, 54.0, 0.0},
		EPSG:      32709,
		Projected: true,
		Zone:      "9S",
	}
	UTMZONE10N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 10N",
		AreaOfUse: "Between 54°E and 60°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{54.0, 0.0, 60.0, 84.0},
		EPSG:      32610,
		Projected: true,
		Zone:      "10N",
	}
	UTMZONE10S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 10S",
		AreaOfUse: "Between 54°E and 60°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{54.0, -80.0, 60.0, 0.0},
		EPSG:      32710,
		Projected: true,
		Zone:      "10S",
	}
	UTMZONE11N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 11N",
		AreaOfUse: "Between 60°E and 66°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{60.0, 0.0, 66.0, 84.0},
		EPSG:      32611,
		Projected: true,
		Zone:      "11N",
	}
	UTMZONE11S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 11S",
		AreaOfUse: "Between 60°E and 66°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{60.0, -80.0, 66.0, 0.0},
		EPSG:      32711,
		Projected: true,
		Zone:      "11S",
	}
	UTMZONE12N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 12N",
		AreaOfUse: "Between 66°E and 72°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{66.0, 0.0, 72.0, 84.0},
		EPSG:      32612,
		Projected: true,
		Zone:      "12N",
	}
	UTMZONE12S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 12S",
		AreaOfUse: "Between 66°E and 72°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{66.0, -80.0, 72.0, 0.0},
		EPSG:      32712,
		Projected: true,
		Zone:      "12S",
	}
	UTMZONE13N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 13N",
		AreaOfUse: "Between 72°E and 78°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{72.0, 0.0, 78.0, 84.0},
		EPSG:      32613,
		Projected: true,
		Zone:      "13N",
	}
	UTMZONE13S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 13S",
		AreaOfUse: "Between 72°E and 78°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{72.0, -80.0, 78.0, 0.0},
		EPSG:      32713,
		Projected: true,
		Zone:      "13S",
	}
	UTMZONE14N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 14N",
		AreaOfUse: "Between 78°E and 84°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{78.0, 0.0, 84.0, 84.0},
		EPSG:      32614,
		Projected: true,
		Zone:      "14N",
	}
	UTMZONE14S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 14S",
		AreaOfUse: "Between 78°E and 84°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{78.0, -80.0, 84.0, 0.0},
		EPSG:      32714,
		Projected: true,
		Zone:      "14S",
	}
	UTMZONE15N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 15N",
		AreaOfUse: "Between 84°E and 90°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{84.0, 0.0, 90.0, 84.0},
		EPSG:      32615,
		Projected: true,
		Zone:      "15N",
	}
	UTMZONE15S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 15S",
		AreaOfUse: "Between 84°E and 90°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{84.0, -80.0, 90.0, 0.0},
		EPSG:      32715,
		Projected: true,
		Zone:      "15S",
	}
	UTMZONE16N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 16N",
		AreaOfUse: "Between 90°E and 96°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{90.0, 0.0, 96.0, 84.0},
		EPSG:      32616,
		Projected: true,
		Zone:      "16N",
	}
	UTMZONE16S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 16S",
		AreaOfUse: "Between 90°E and 96°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{90.0, -80.0, 96.0, 0.0},
		EPSG:      32716,
		Projected: true,
		Zone:      "16S",
	}
	UTMZONE17N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 17N",
		AreaOfUse: "Between 96°E and 102°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{96.0, 0.0, 102.0, 84.0},
		EPSG:      32617,
		Projected: true,
		Zone:      "17N",
	}
	UTMZONE17S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 17S",
		AreaOfUse: "Between 96°E and 102°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{96.0, -80.0, 102.0, 0.0},
		EPSG:      32717,
		Projected: true,
		Zone:      "17S",
	}
	UTMZONE18N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 18N",
		AreaOfUse: "Between 102°E and 108°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{102.0, 0.0, 108.0, 84.0},
		EPSG:      32618,
		Projected: true,
		Zone:      "18N",
	}
	UTMZONE18S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 18S",
		AreaOfUse: "Between 102°E and 108°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{102.0, -80.0, 108.0, 0.0},
		EPSG:      32718,
		Projected: true,
		Zone:      "18S",
	}
	UTMZONE19N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 19N",
		AreaOfUse: "Between 108°E and 114°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{108.0, 0.0, 114.0, 84.0},
		EPSG:      32619,
		Projected: true,
		Zone:      "19N",
	}
	UTMZONE19S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 19S",
		AreaOfUse: "Between 108°E and 114°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{108.0, -80.0, 114.0, 0.0},
		EPSG:      32719,
		Projected: true,
		Zone:      "19S",
	}
	UTMZONE20N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 20N",
		AreaOfUse: "Between 114°E and 120°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{114.0, 0.0, 120.0, 84.0},
		EPSG:      32620,
		Projected: true,
		Zone:      "20N",
	}
	UTMZONE20S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 20S",
		AreaOfUse: "Between 114°E and 120°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{114.0, -80.0, 120.0, 0.0},
		EPSG:      32720,
		Projected: true,
		Zone:      "20S",
	}
	UTMZONE21N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 21N",
		AreaOfUse: "Between 120°E and 126°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{120.0, 0.0, 126.0, 84.0},
		EPSG:      32621,
		Projected: true,
		Zone:      "21N",
	}
	UTMZONE21S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 21S",
		AreaOfUse: "Between 120°E and 126°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{120.0, -80.0, 126.0, 0.0},
		EPSG:      32721,
		Projected: true,
		Zone:      "21S",
	}
	UTMZONE22N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 22N",
		AreaOfUse: "Between 126°E and 132°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{126.0, 0.0, 132.0, 84.0},
		EPSG:      32622,
		Projected: true,
		Zone:      "22N",
	}
	UTMZONE22S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 22S",
		AreaOfUse: "Between 126°E and 132°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{126.0, -80.0, 132.0, 0.0},
		EPSG:      32722,
		Projected: true,
		Zone:      "22S",
	}
	UTMZONE23N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 23N",
		AreaOfUse: "Between 132°E and 138°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{132.0, 0.0, 138.0, 84.0},
		EPSG:      32623,
		Projected: true,
		Zone:      "23N",
	}
	UTMZONE23S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 23S",
		AreaOfUse: "Between 132°E and 138°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{132.0, -80.0, 138.0, 0.0},
		EPSG:      32723,
		Projected: true,
		Zone:      "23S",
	}
	UTMZONE24N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 24N",
		AreaOfUse: "Between 138°E and 144°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{138.0, 0.0, 144.0, 84.0},
		EPSG:      32624,
		Projected: true,
		Zone:      "24N",
	}
	UTMZONE24S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 24S",
		AreaOfUse: "Between 138°E and 144°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{138.0, -80.0, 144.0, 0.0},
		EPSG:      32724,
		Projected: true,
		Zone:      "24S",
	}
	UTMZONE25N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 25N",
		AreaOfUse: "Between 144°E and 150°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{144.0, 0.0, 150.0, 84.0},
		EPSG:      32625,
		Projected: true,
		Zone:      "25N",
	}
	UTMZONE25S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 25S",
		AreaOfUse: "Between 144°E and 150°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{144.0, -80.0, 150.0, 0.0},
		EPSG:      32725,
		Projected: true,
		Zone:      "25S",
	}
	UTMZONE26N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 26N",
		AreaOfUse: "Between 150°E and 156°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{150.0, 0.0, 156.0, 84.0},
		EPSG:      32626,
		Projected: true,
		Zone:      "26N",
	}
	UTMZONE26S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 26S",
		AreaOfUse: "Between 150°E and 156°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{150.0, -80.0, 156.0, 0.0},
		EPSG:      32726,
		Projected: true,
		Zone:      "26S",
	}
	UTMZONE27N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 27N",
		AreaOfUse: "Between 156°E and 162°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{156.0, 0.0, 162.0, 84.0},
		EPSG:      32627,
		Projected: true,
		Zone:      "27N",
	}
	UTMZONE27S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 27S",
		AreaOfUse: "Between 156°E and 162°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{156.0, -80.0, 162.0, 0.0},
		EPSG:      32727,
		Projected: true,
		Zone:      "27S",
	}
	UTMZONE28N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 28N",
		AreaOfUse: "Between 162°E and 168°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{162.0, 0.0, 168.0, 84.0},
		EPSG:      32628,
		Projected: true,
		Zone:      "28N",
	}
	UTMZONE28S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 28S",
		AreaOfUse: "Between 162°E and 168°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{162.0, -80.0, 168.0, 0.0},
		EPSG:      32728,
		Projected: true,
		Zone:      "28S",
	}
	UTMZONE29N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 29N",
		AreaOfUse: "Between 168°E and 174°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{168.0, 0.0, 174.0, 84.0},
		EPSG:      32629,
		Projected: true,
		Zone:      "29N",
	}
	UTMZONE29S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 29S",
		AreaOfUse: "Between 168°E and 174°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{168.0, -80.0, 174.0, 0.0},
		EPSG:      32729,
		Projected: true,
		Zone:      "29S",
	}
	UTMZONE30N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 30N",
		AreaOfUse: "Between 174°E and 180°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{174.0, 0.0, 180.0, 84.0},
		EPSG:      32630,
		Projected: true,
		Zone:      "30N",
	}
	UTMZONE30S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 30S",
		AreaOfUse: "Between 174°E and 180°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{174.0, -80.0, 180.0, 0.0},
		EPSG:      32730,
		Projected: true,
		Zone:      "30S",
	}
	UTMZONE31N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 31N",
		AreaOfUse: "Between 180°E and 186°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{180.0, 0.0, 186.0, 84.0},
		EPSG:      32631,
		Projected: true,
		Zone:      "31N",
	}
	UTMZONE31S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 31S",
		AreaOfUse: "Between 180°E and 186°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{180.0, -80.0, 186.0, 0.0},
		EPSG:      32731,
		Projected: true,
		Zone:      "31S",
	}
	UTMZONE32N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 32N",
		AreaOfUse: "Between 186°E and 192°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{186.0, 0.0, 192.0, 84.0},
		EPSG:      32632,
		Projected: true,
		Zone:      "32N",
	}
	UTMZONE32S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 32S",
		AreaOfUse: "Between 186°E and 192°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{186.0, -80.0, 192.0, 0.0},
		EPSG:      32732,
		Projected: true,
		Zone:      "32S",
	}
	UTMZONE33N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 33N",
		AreaOfUse: "Between 192°E and 198°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{192.0, 0.0, 198.0, 84.0},
		EPSG:      32633,
		Projected: true,
		Zone:      "33N",
	}
	UTMZONE33S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 33S",
		AreaOfUse: "Between 192°E and 198°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{192.0, -80.0, 198.0, 0.0},
		EPSG:      32733,
		Projected: true,
		Zone:      "33S",
	}
	UTMZONE34N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 34N",
		AreaOfUse: "Between 198°E and 204°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{198.0, 0.0, 204.0, 84.0},
		EPSG:      32634,
		Projected: true,
		Zone:      "34N",
	}
	UTMZONE34S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 34S",
		AreaOfUse: "Between 198°E and 204°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{198.0, -80.0, 204.0, 0.0},
		EPSG:      32734,
		Projected: true,
		Zone:      "34S",
	}
	UTMZONE35N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 35N",
		AreaOfUse: "Between 204°E and 210°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{204.0, 0.0, 210.0, 84.0},
		EPSG:      32635,
		Projected: true,
		Zone:      "35N",
	}
	UTMZONE35S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 35S",
		AreaOfUse: "Between 204°E and 210°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{204.0, -80.0, 210.0, 0.0},
		EPSG:      32735,
		Projected: true,
		Zone:      "35S",
	}
	UTMZONE36N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 36N",
		AreaOfUse: "Between 210°E and 216°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{210.0, 0.0, 216.0, 84.0},
		EPSG:      32636,
		Projected: true,
		Zone:      "36N",
	}
	UTMZONE36S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 36S",
		AreaOfUse: "Between 210°E and 216°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{210.0, -80.0, 216.0, 0.0},
		EPSG:      32736,
		Projected: true,
		Zone:      "36S",
	}
	UTMZONE37N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 37N",
		AreaOfUse: "Between 216°E and 222°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{216.0, 0.0, 222.0, 84.0},
		EPSG:      32637,
		Projected: true,
		Zone:      "37N",
	}
	UTMZONE37S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 37S",
		AreaOfUse: "Between 216°E and 222°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{216.0, -80.0, 222.0, 0.0},
		EPSG:      32737,
		Projected: true,
		Zone:      "37S",
	}
	UTMZONE38N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 38N",
		AreaOfUse: "Between 222°E and 228°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{222.0, 0.0, 228.0, 84.0},
		EPSG:      32638,
		Projected: true,
		Zone:      "38N",
	}
	UTMZONE38S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 38S",
		AreaOfUse: "Between 222°E and 228°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{222.0, -80.0, 228.0, 0.0},
		EPSG:      32738,
		Projected: true,
		Zone:      "38S",
	}
	UTMZONE39N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 39N",
		AreaOfUse: "Between 228°E and 234°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{228.0, 0.0, 234.0, 84.0},
		EPSG:      32639,
		Projected: true,
		Zone:      "39N",
	}
	UTMZONE39S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 39S",
		AreaOfUse: "Between 228°E and 234°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{228.0, -80.0, 234.0, 0.0},
		EPSG:      32739,
		Projected: true,
		Zone:      "39S",
	}
	UTMZONE40N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 40N",
		AreaOfUse: "Between 234°E and 240°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{234.0, 0.0, 240.0, 84.0},
		EPSG:      32640,
		Projected: true,
		Zone:      "40N",
	}
	UTMZONE40S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 40S",
		AreaOfUse: "Between 234°E and 240°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{234.0, -80.0, 240.0, 0.0},
		EPSG:      32740,
		Projected: true,
		Zone:      "40S",
	}
	UTMZONE41N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 41N",
		AreaOfUse: "Between 240°E and 246°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{240.0, 0.0, 246.0, 84.0},
		EPSG:      32641,
		Projected: true,
		Zone:      "41N",
	}
	UTMZONE41S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 41S",
		AreaOfUse: "Between 240°E and 246°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{240.0, -80.0, 246.0, 0.0},
		EPSG:      32741,
		Projected: true,
		Zone:      "41S",
	}
	UTMZONE42N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 42N",
		AreaOfUse: "Between 246°E and 252°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{246.0, 0.0, 252.0, 84.0},
		EPSG:      32642,
		Projected: true,
		Zone:      "42N",
	}
	UTMZONE42S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 42S",
		AreaOfUse: "Between 246°E and 252°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{246.0, -80.0, 252.0, 0.0},
		EPSG:      32742,
		Projected: true,
		Zone:      "42S",
	}
	UTMZONE43N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 43N",
		AreaOfUse: "Between 252°E and 258°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{252.0, 0.0, 258.0, 84.0},
		EPSG:      32643,
		Projected: true,
		Zone:      "43N",
	}
	UTMZONE43S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 43S",
		AreaOfUse: "Between 252°E and 258°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{252.0, -80.0, 258.0, 0.0},
		EPSG:      32743,
		Projected: true,
		Zone:      "43S",
	}
	UTMZONE44N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 44N",
		AreaOfUse: "Between 258°E and 264°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{258.0, 0.0, 264.0, 84.0},
		EPSG:      32644,
		Projected: true,
		Zone:      "44N",
	}
	UTMZONE44S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 44S",
		AreaOfUse: "Between 258°E and 264°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{258.0, -80.0, 264.0, 0.0},
		EPSG:      32744,
		Projected: true,
		Zone:      "44S",
	}
	UTMZONE45N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 45N",
		AreaOfUse: "Between 264°E and 270°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{264.0, 0.0, 270.0, 84.0},
		EPSG:      32645,
		Projected: true,
		Zone:      "45N",
	}
	UTMZONE45S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 45S",
		AreaOfUse: "Between 264°E and 270°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{264.0, -80.0, 270.0, 0.0},
		EPSG:      32745,
		Projected: true,
		Zone:      "45S",
	}
	UTMZONE46N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 46N",
		AreaOfUse: "Between 270°E and 276°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{270.0, 0.0, 276.0, 84.0},
		EPSG:      32646,
		Projected: true,
		Zone:      "46N",
	}
	UTMZONE46S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 46S",
		AreaOfUse: "Between 270°E and 276°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{270.0, -80.0, 276.0, 0.0},
		EPSG:      32746,
		Projected: true,
		Zone:      "46S",
	}
	UTMZONE47N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 47N",
		AreaOfUse: "Between 276°E and 282°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{276.0, 0.0, 282.0, 84.0},
		EPSG:      32647,
		Projected: true,
		Zone:      "47N",
	}
	UTMZONE47S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 47S",
		AreaOfUse: "Between 276°E and 282°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{276.0, -80.0, 282.0, 0.0},
		EPSG:      32747,
		Projected: true,
		Zone:      "47S",
	}
	UTMZONE48N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 48N",
		AreaOfUse: "Between 282°E and 288°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{282.0, 0.0, 288.0, 84.0},
		EPSG:      32648,
		Projected: true,
		Zone:      "48N",
	}
	UTMZONE48S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 48S",
		AreaOfUse: "Between 282°E and 288°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{282.0, -80.0, 288.0, 0.0},
		EPSG:      32748,
		Projected: true,
		Zone:      "48S",
	}
	UTMZONE49N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 49N",
		AreaOfUse: "Between 288°E and 294°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{288.0, 0.0, 294.0, 84.0},
		EPSG:      32649,
		Projected: true,
		Zone:      "49N",
	}
	UTMZONE49S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 49S",
		AreaOfUse: "Between 288°E and 294°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{288.0, -80.0, 294.0, 0.0},
		EPSG:      32749,
		Projected: true,
		Zone:      "49S",
	}
	UTMZONE50N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 50N",
		AreaOfUse: "Between 294°E and 300°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{294.0, 0.0, 300.0, 84.0},
		EPSG:      32650,
		Projected: true,
		Zone:      "50N",
	}
	UTMZONE50S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 50S",
		AreaOfUse: "Between 294°E and 300°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{294.0, -80.0, 300.0, 0.0},
		EPSG:      32750,
		Projected: true,
		Zone:      "50S",
	}
	UTMZONE51N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 51N",
		AreaOfUse: "Between 300°E and 306°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{300.0, 0.0, 306.0, 84.0},
		EPSG:      32651,
		Projected: true,
		Zone:      "51N",
	}
	UTMZONE51S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 51S",
		AreaOfUse: "Between 300°E and 306°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{300.0, -80.0, 306.0, 0.0},
		EPSG:      32751,
		Projected: true,
		Zone:      "51S",
	}
	UTMZONE52N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 52N",
		AreaOfUse: "Between 306°E and 312°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{306.0, 0.0, 312.0, 84.0},
		EPSG:      32652,
		Projected: true,
		Zone:      "52N",
	}
	UTMZONE52S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 52S",
		AreaOfUse: "Between 306°E and 312°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{306.0, -80.0, 312.0, 0.0},
		EPSG:      32752,
		Projected: true,
		Zone:      "52S",
	}
	UTMZONE53N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 53N",
		AreaOfUse: "Between 312°E and 318°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{312.0, 0.0, 318.0, 84.0},
		EPSG:      32653,
		Projected: true,
		Zone:      "53N",
	}
	UTMZONE53S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 53S",
		AreaOfUse: "Between 312°E and 318°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{312.0, -80.0, 318.0, 0.0},
		EPSG:      32753,
		Projected: true,
		Zone:      "53S",
	}
	UTMZONE54N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 54N",
		AreaOfUse: "Between 318°E and 324°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{318.0, 0.0, 324.0, 84.0},
		EPSG:      32654,
		Projected: true,
		Zone:      "54N",
	}
	UTMZONE54S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 54S",
		AreaOfUse: "Between 318°E and 324°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{318.0, -80.0, 324.0, 0.0},
		EPSG:      32754,
		Projected: true,
		Zone:      "54S",
	}
	UTMZONE55N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 55N",
		AreaOfUse: "Between 324°E and 330°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{324.0, 0.0, 330.0, 84.0},
		EPSG:      32655,
		Projected: true,
		Zone:      "55N",
	}
	UTMZONE55S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 55S",
		AreaOfUse: "Between 324°E and 330°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{324.0, -80.0, 330.0, 0.0},
		EPSG:      32755,
		Projected: true,
		Zone:      "55S",
	}
	UTMZONE56N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 56N",
		AreaOfUse: "Between 330°E and 336°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{330.0, 0.0, 336.0, 84.0},
		EPSG:      32656,
		Projected: true,
		Zone:      "56N",
	}
	UTMZONE56S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 56S",
		AreaOfUse: "Between 330°E and 336°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{330.0, -80.0, 336.0, 0.0},
		EPSG:      32756,
		Projected: true,
		Zone:      "56S",
	}
	UTMZONE57N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 57N",
		AreaOfUse: "Between 336°E and 342°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{336.0, 0.0, 342.0, 84.0},
		EPSG:      32657,
		Projected: true,
		Zone:      "57N",
	}
	UTMZONE57S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 57S",
		AreaOfUse: "Between 336°E and 342°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{336.0, -80.0, 342.0, 0.0},
		EPSG:      32757,
		Projected: true,
		Zone:      "57S",
	}
	UTMZONE58N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 58N",
		AreaOfUse: "Between 342°E and 348°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{342.0, 0.0, 348.0, 84.0},
		EPSG:      32658,
		Projected: true,
		Zone:      "58N",
	}
	UTMZONE58S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 58S",
		AreaOfUse: "Between 342°E and 348°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{342.0, -80.0, 348.0, 0.0},
		EPSG:      32758,
		Projected: true,
		Zone:      "58S",
	}
	UTMZONE59N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 59N",
		AreaOfUse: "Between 348°E and 354°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{348.0, 0.0, 354.0, 84.0},
		EPSG:      32659,
		Projected: true,
		Zone:      "59N",
	}
	UTMZONE59S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 59S",
		AreaOfUse: "Between 348°E and 354°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{348.0, -80.0, 354.0, 0.0},
		EPSG:      32759,
		Projected: true,
		Zone:      "59S",
	}
	UTMZONE60N CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 60N",
		AreaOfUse: "Between 354°E and 360°E, northern hemisphere between equator and 84°N",
		BoundBox:  [4]float64{354.0, 0.0, 360.0, 84.0},
		EPSG:      32660,
		Projected: true,
		Zone:      "60N",
	}
	UTMZONE60S CRS = CRS{
		Name:      "WGS 84 / UTM ZONE 60S",
		AreaOfUse: "Between 354°E and 360°E, southern hemisphere between 80°S and equator",
		BoundBox:  [4]float64{354.0, -80.0, 360.0, 0.0},
		EPSG:      32760,
		Projected: true,
		Zone:      "60S",
	}
)

var CRSbyEPSG map[int]CRS = map[int]CRS{
	4326:  WGS84,
	3857:  PSEUDOMERCATOR,
	32601: UTMZONE1N,
	32701: UTMZONE1S,
	32602: UTMZONE2N,
	32702: UTMZONE2S,
	32603: UTMZONE3N,
	32703: UTMZONE3S,
	32604: UTMZONE4N,
	32704: UTMZONE4S,
	32605: UTMZONE5N,
	32705: UTMZONE5S,
	32606: UTMZONE6N,
	32706: UTMZONE6S,
	32607: UTMZONE7N,
	32707: UTMZONE7S,
	32608: UTMZONE8N,
	32708: UTMZONE8S,
	32609: UTMZONE9N,
	32709: UTMZONE9S,
	32610: UTMZONE10N,
	32710: UTMZONE10S,
	32611: UTMZONE11N,
	32711: UTMZONE11S,
	32612: UTMZONE12N,
	32712: UTMZONE12S,
	32613: UTMZONE13N,
	32713: UTMZONE13S,
	32614: UTMZONE14N,
	32714: UTMZONE14S,
	32615: UTMZONE15N,
	32715: UTMZONE15S,
	32616: UTMZONE16N,
	32716: UTMZONE16S,
	32617: UTMZONE17N,
	32717: UTMZONE17S,
	32618: UTMZONE18N,
	32718: UTMZONE18S,
	32619: UTMZONE19N,
	32719: UTMZONE19S,
	32620: UTMZONE20N,
	32720: UTMZONE20S,
	32621: UTMZONE21N,
	32721: UTMZONE21S,
	32622: UTMZONE22N,
	32722: UTMZONE22S,
	32623: UTMZONE23N,
	32723: UTMZONE23S,
	32624: UTMZONE24N,
	32724: UTMZONE24S,
	32625: UTMZONE25N,
	32725: UTMZONE25S,
	32626: UTMZONE26N,
	32726: UTMZONE26S,
	32627: UTMZONE27N,
	32727: UTMZONE27S,
	32628: UTMZONE28N,
	32728: UTMZONE28S,
	32629: UTMZONE29N,
	32729: UTMZONE29S,
	32630: UTMZONE30N,
	32730: UTMZONE30S,
	32631: UTMZONE31N,
	32731: UTMZONE31S,
	32632: UTMZONE32N,
	32732: UTMZONE32S,
	32633: UTMZONE33N,
	32733: UTMZONE33S,
	32634: UTMZONE34N,
	32734: UTMZONE34S,
	32635: UTMZONE35N,
	32735: UTMZONE35S,
	32636: UTMZONE36N,
	32736: UTMZONE36S,
	32637: UTMZONE37N,
	32737: UTMZONE37S,
	32638: UTMZONE38N,
	32738: UTMZONE38S,
	32639: UTMZONE39N,
	32739: UTMZONE39S,
	32640: UTMZONE40N,
	32740: UTMZONE40S,
	32641: UTMZONE41N,
	32741: UTMZONE41S,
	32642: UTMZONE42N,
	32742: UTMZONE42S,
	32643: UTMZONE43N,
	32743: UTMZONE43S,
	32644: UTMZONE44N,
	32744: UTMZONE44S,
	32645: UTMZONE45N,
	32745: UTMZONE45S,
	32646: UTMZONE46N,
	32746: UTMZONE46S,
	32647: UTMZONE47N,
	32747: UTMZONE47S,
	32648: UTMZONE48N,
	32748: UTMZONE48S,
	32649: UTMZONE49N,
	32749: UTMZONE49S,
	32650: UTMZONE50N,
	32750: UTMZONE50S,
	32651: UTMZONE51N,
	32751: UTMZONE51S,
	32652: UTMZONE52N,
	32752: UTMZONE52S,
	32653: UTMZONE53N,
	32753: UTMZONE53S,
	32654: UTMZONE54N,
	32754: UTMZONE54S,
	32655: UTMZONE55N,
	32755: UTMZONE55S,
	32656: UTMZONE56N,
	32756: UTMZONE56S,
	32657: UTMZONE57N,
	32757: UTMZONE57S,
	32658: UTMZONE58N,
	32758: UTMZONE58S,
	32659: UTMZONE59N,
	32759: UTMZONE59S,
	32660: UTMZONE60N,
	32760: UTMZONE60S,
}
