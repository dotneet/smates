import os

env = dict(os.environ)  # copying the environment is not a read of every variable
assert isinstance(env, dict)
print("plain")
