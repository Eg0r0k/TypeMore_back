import { Controller, Get, Req, Res, UseGuards } from '@nestjs/common';
import { UsersService } from './users.service';
import { Request, Response } from 'express';
import { JwtAuthGuard } from '../auth/auth.guard';

@Controller('users')
export class UsersController {
  constructor(private readonly usersService: UsersService) {}
  @Get()
  @UseGuards(JwtAuthGuard)
  async getAllUsers(@Req() request: Request, @Res() response: Response) {
    try {
      const result = await this.usersService.getAllUser();
      return response.status(200).json({
        status: 'OK',
        message: 'Seccesfully data',
        result: result
      });
    } catch (error) {
      return response.status(500).json({
        status: 'OK',
        message: 'Server Error'
      });
    }
  }
}
