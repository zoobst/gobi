import pandas as pd
import polars as pl
import geopandas as gpd

def main():
    df = pl.read_parquet('testData/titanic_test.parquet')
    # df = pd.read_csv()
    # df = gpd.read_file()
    df.write_json('testData/titanic_test.json')

if __name__ == "__main__":
    main()