"""Real-time budget guardrail: freeze autoscaling at the monthly limit."""
from dataclasses import dataclass


@dataclass
class BudgetGuardrail:
    monthly_limit: float
    spent: float = 0.0

    def record(self, amount: float) -> None:
        if amount < 0:
            raise ValueError("amount must be non-negative")
        self.spent += amount

    @property
    def frozen(self) -> bool:
        return self.spent >= self.monthly_limit

    def allows(self, action: str) -> bool:
        """Frozen budgets block growth; shrinking (saving money) is always allowed."""
        return not (self.frozen and action == "grow")
