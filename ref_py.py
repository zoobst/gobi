import pandas as pd
import polars as pl
import geopandas as gpd

def main():
    df = pl.read_parquet('testData/titanic.parquet')
    # df = pd.read_csv()
    # df = gpd.read_file()
    df.write_parquet('testData/titanic_test.gz.parquet', compression="gzip")

if __name__ == "__main__":
    main()