import unittest

policy = None


def setUpModule():
    global policy
    from library import policy as p

    policy = p


class PolicyTest(unittest.TestCase):
    def test_values(self):
        self.assertEqual((policy.MAX_LOANS, policy.LATE_FEE_CENTS), (3, 25))

    @unittest.skip("fees are not capped yet")
    def test_fee_cap(self):
        pass

    @unittest.expectedFailure
    def test_no_free_loans(self):
        self.assertEqual(policy.LATE_FEE_CENTS, 0)
