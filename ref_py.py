import pandas as pd
import polars as pl
import geopandas as gpd
from shapely.geometry import Point, Polygon, LineString
import pyproj

def main():
    df = pl.read_parquet('testData/titanic.parquet')
    # df = pd.read_csv()
    # df = gpd.read_file()
    # df.write_parquet('testData/titanic_test.gz.parquet', compression="gzip")
    df.write_csv("testData/titanic_test.csv", datetime_format="%Y-%m-%d %H:%M:%S")
    gpd.GeoDataFrame().crs
    pyproj.CRS()

if __name__ == "__main__":
    main()