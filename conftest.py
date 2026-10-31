"""webtouch 根级 pytest 配置。

仅用于把项目根加入 sys.path（rootdir conftest 自动被 pytest 导入），
使 tests/ 下用例能以 `from learning.xxx import ...` 方式导入实现包。
"""
