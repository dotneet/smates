import os

region = os.environ.get("APP_REGION", "default")
assert isinstance(region, str)
print("region:", region != "")
