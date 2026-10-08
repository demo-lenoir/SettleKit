// SPDX-License-Identifier: MIT
pragma solidity 0.8.37;

import {Test} from "forge-std/Test.sol";
import {PaymentEscrow} from "../src/PaymentEscrow.sol";
import {MockUSDC} from "./PaymentEscrow.t.sol";

contract EscrowHandler is Test {
    address internal constant PAYER = address(0xD00D);
    address internal constant PAYEE = address(0xE00E);
    address internal constant OPERATOR = address(0xB0B);

    PaymentEscrow public immutable escrow;
    bytes32[] public ids;
    uint256 public deposited;
    uint256 public paid;

    constructor(PaymentEscrow escrow_) {
        escrow = escrow_;
    }

    function count() external view returns (uint256) {
        return ids.length;
    }

    function fund(bytes16 intent, uint96 rawAmount, uint32 rawDuration) external {
        if (ids.length >= 32) return;
        uint128 amount = uint128(bound(rawAmount, 1, 1e6));
        uint64 deadline = uint64(block.timestamp + bound(rawDuration, 1, 30 days));
        vm.prank(PAYER);
        try escrow.createAndFund(intent, PAYEE, amount, deadline) returns (bytes32 id) {
            ids.push(id);
            deposited += amount;
        } catch {}
    }

    function release(uint256 rawIndex) external {
        if (ids.length == 0) return;
        bytes32 id = ids[rawIndex % ids.length];
        (,, uint128 amount,,) = escrow.escrows(id);
        vm.prank(OPERATOR);
        try escrow.release(id) {
            paid += amount;
        } catch {}
    }

    function refund(uint256 rawIndex) external {
        if (ids.length == 0) return;
        bytes32 id = ids[rawIndex % ids.length];
        (,, uint128 amount,,) = escrow.escrows(id);
        vm.prank(OPERATOR);
        try escrow.refund(id) {
            paid += amount;
        } catch {}
    }

    function expire(uint256 rawIndex) external {
        if (ids.length == 0) return;
        bytes32 id = ids[rawIndex % ids.length];
        (,, uint128 amount, uint64 deadline,) = escrow.escrows(id);
        if (block.timestamp < deadline) vm.warp(deadline);
        try escrow.claimExpiredRefund(id) {
            paid += amount;
        } catch {}
    }
}

contract PaymentEscrowInvariant is Test {
    address internal constant ADMIN = address(0xA11CE);
    address internal constant OPERATOR = address(0xB0B);
    address internal constant PAUSER = address(0xCAFE);
    address internal constant PAYER = address(0xD00D);

    MockUSDC internal token;
    PaymentEscrow internal escrow;
    EscrowHandler internal handler;

    function setUp() public {
        token = new MockUSDC();
        escrow = new PaymentEscrow(address(token), ADMIN, OPERATOR, PAUSER);
        handler = new EscrowHandler(escrow);
        token.mint(PAYER, 1_000_000e6);
        vm.prank(PAYER);
        token.approve(address(escrow), type(uint256).max);

        bytes4[] memory selectors = new bytes4[](4);
        selectors[0] = EscrowHandler.fund.selector;
        selectors[1] = EscrowHandler.release.selector;
        selectors[2] = EscrowHandler.refund.selector;
        selectors[3] = EscrowHandler.expire.selector;
        targetContract(address(handler));
        targetSelector(FuzzSelector({addr: address(handler), selectors: selectors}));
    }

    function invariant_conservationAndTerminalExclusivity() public view {
        uint256 fundedLiability;
        uint256 n = handler.count();
        for (uint256 i; i < n; ++i) {
            (,, uint128 amount,, PaymentEscrow.Status status) = escrow.escrows(handler.ids(i));
            if (status == PaymentEscrow.Status.FUNDED) fundedLiability += amount;
            else assertTrue(status == PaymentEscrow.Status.RELEASED || status == PaymentEscrow.Status.REFUNDED);
        }
        assertLe(handler.paid(), handler.deposited());
        assertEq(handler.paid() + fundedLiability, handler.deposited());
        assertEq(token.balanceOf(address(escrow)), fundedLiability);
    }
}
