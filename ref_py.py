import pandas as pd
import polars as pl
import geopandas as gpd

def main():
    df = pl.read_parquet('testData/titanic.parquet')
    # df = pd.read_csv()
    # df = gpd.read_file()
    # df.write_parquet('testData/titanic_test.gz.parquet', compression="gzip")
    df.write_csv("testData/titanic_test.csv", datetime_format="%Y-%m-%d %H:%M:%S")

if __name__ == "__main__":
    main()